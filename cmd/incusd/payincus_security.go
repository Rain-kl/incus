package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"

	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/instancetype"
	nodesecurity "github.com/lxc/incus/v7/internal/server/payincus/security"
	"github.com/lxc/incus/v7/shared/logger"
)

// PayIncus: the audit socket exposes only event submission and authenticates local UID/PID via the kernel.
func startPayIncusAudit(d *Daemon) {
	manager := nodesecurity.Default()
	installPayIncusStorageWorkloads(d)
	manager.SetSnapshot(func(ctx context.Context) ([]nodesecurity.ResumeInstance, error) {
		instances, err := instance.LoadNodeAll(d.State(), instancetype.Any)
		if err != nil {
			return nil, err
		}
		pending := []nodesecurity.ResumeInstance{}
		for _, inst := range instances {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if inst.IsRunning() {
				pending = append(pending, nodesecurity.ResumeInstance{Project: inst.Project().Name, Name: inst.Name(), UUID: inst.LocalConfig()["volatile.uuid"], Frozen: inst.IsFrozen()})
			}
		}
		return pending, nil
	})
	// PayIncus: recover native workload identities after normal restarts, including crash power-state records.
	manager.SetStartupRecovery(func(ctx context.Context) ([]nodesecurity.ResumeInstance, error) {
		instances, err := instance.LoadNodeAll(d.State(), instancetype.Any)
		if err != nil {
			return nil, err
		}
		pending := []nodesecurity.ResumeInstance{}
		for _, inst := range instances {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			config := inst.ExpandedConfig()
			if config["boot.autostart"] == "true" || (config["boot.autostart"] != "false" && inst.LocalConfig()["volatile.last_state.power"] == "RUNNING") {
				pending = append(pending, nodesecurity.ResumeInstance{Project: inst.Project().Name, Name: inst.Name(), UUID: inst.LocalConfig()["volatile.uuid"], Frozen: inst.IsFrozen()})
			}
		}
		return pending, nil
	}, func(ctx context.Context, item nodesecurity.ResumeInstance) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		inst, err := instance.LoadByProjectAndName(d.State(), item.Project, item.Name)
		if err != nil {
			return err
		}
		if inst.LocalConfig()["volatile.uuid"] != item.UUID {
			return errors.New("workload identity changed")
		}
		if !inst.IsRunning() {
			err = inst.Start(false)
			if err != nil {
				return err
			}
		}
		if item.Frozen && !inst.IsFrozen() {
			return inst.Freeze()
		}
		return nil
	})
	manager.SetStop(func(ctx context.Context) error {
		instances, err := instance.LoadNodeAll(d.State(), instancetype.Any)
		if err != nil {
			return err
		}
		var group errgroup.Group
		group.SetLimit(16)
		for _, inst := range instances {
			if !inst.IsRunning() {
				continue
			}
			group.Go(func() error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return inst.Stop(false)
			})
		}
		stopErr := group.Wait()
		for _, inst := range instances {
			if inst.IsRunning() {
				return errors.New("instance remains running")
			}
		}
		return stopErr
	})
	socket := filepath.Join(filepath.Dir(nodesecurity.Directory()), "payincus-audit.socket")
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		logger.Error("Node initialization failed")
		return
	}
	err = os.Chmod(socket, 0o666)
	if err != nil {
		_ = listener.Close()
		return
	}
	server := &http.Server{ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 4096}
	server.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		conn, ok := c.(*net.UnixConn)
		if !ok {
			return ctx
		}
		raw, err := conn.SyscallConn()
		if err != nil {
			return ctx
		}
		var credential *unix.Ucred
		_ = raw.Control(func(fd uintptr) { credential, _ = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		return context.WithValue(ctx, auditPeer{}, credential)
	}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		credential, ok := r.Context().Value(auditPeer{}).(*unix.Ucred)
		if !ok || credential == nil || r.Method != "POST" || r.URL.Path != "/audit" {
			http.Error(w, "cannot authenticate local CLI audit request", http.StatusForbidden)
			return
		}
		var input struct {
			Args []string `json:"args"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, 4097))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || len(input.Args) != 1 || decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "invalid local CLI audit request", http.StatusBadRequest)
			return
		}
		category, critical := nodesecurity.Command(input.Args)
		blocked, err := manager.Audit(category, critical, int(credential.Uid), int(credential.Pid))
		if err != nil {
			// PayIncus: an audit write failure is not proof of a persisted security lock.
			if errors.Is(manager.Check(), nodesecurity.ErrLocked) {
				http.Error(w, nodesecurity.PublicError().Error(), http.StatusInternalServerError)
				return
			}
			http.Error(w, "local CLI audit failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if blocked {
			http.Error(w, nodesecurity.PublicError().Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(200)
	})
	go func() { _ = server.Serve(listener) }()
	go func() { <-d.shutdownCtx.Done(); _ = server.Close(); _ = os.Remove(socket) }()
}

type auditPeer struct{}
