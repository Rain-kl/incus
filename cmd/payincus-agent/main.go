// payincus-agent carries authenticated connections; node operations live in incusd.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/config"
	"github.com/lxc/incus/v7/internal/server/payincus/nodeclient"
	"github.com/lxc/incus/v7/internal/server/payincus/panel"
	"github.com/lxc/incus/v7/internal/server/payincus/tunnel"
)

var version = "v1.3.0"

func main() {
	path := flag.String("config", "/etc/incudal-agent/config.yaml", "panel connection configuration")
	incusDir := os.Getenv("INCUS_DIR")
	if incusDir == "" {
		incusDir = "/var/lib/incus"
	}

	socket := flag.String("incus-socket", filepath.Join(incusDir, "unix.socket"), "local Incus socket")
	once := flag.Bool("once", false, "send one heartbeat and exit")
	flag.Parse()
	cfg, err := config.Load(*path)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	local := nodeclient.New(*socket)
	remote := panel.New(cfg)
	worker := tunnel.NewWorker(tunnel.WorkerConfig{PanelURL: cfg.PanelURL, AgentID: cfg.AgentID, AgentSecret: cfg.AgentSecret, ControlHandler: local.Control, NodeClient: local})
	defer worker.SyncConfig(false, "", 0)
	var caddyPending atomic.Bool
	tick := time.NewTicker(cfg.HeartbeatInterval)
	defer tick.Stop()
	for {
		err = heartbeat(ctx, local, remote, cfg, worker, &caddyPending)
		if err != nil {
			log.Printf("heartbeat: %v", err)
		}

		if *once {
			if err != nil {
				os.Exit(1)
			}

			return
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func heartbeat(ctx context.Context, local *nodeclient.Client, remote *panel.Client, cfg config.Config, worker *tunnel.Worker, caddyPending *atomic.Bool) error {
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := local.Check(requestCtx)
	if err != nil {
		return err
	}

	var payload map[string]any
	err = local.Call(requestCtx, "GET", fmt.Sprintf("/report?interval=%d", cfg.HeartbeatIntervalSeconds), nil, &payload)
	if err != nil {
		return err
	}

	payload["version"] = version
	result, err := remote.Heartbeat(requestCtx, payload)
	if err != nil {
		return err
	}

	if result.Tunnel != nil {
		worker.SyncConfig(result.Tunnel.Enabled, result.Tunnel.TargetHost, result.Tunnel.TargetPort)
	}
	// Package-managed binaries must never be replaced by the legacy Agent self-updater.
	if result.Upgrade != nil && result.Upgrade.Available {
		log.Print("node upgrade requested; upgrade the custom Incus package")
	}

	if result.Caddy != nil && (result.Caddy.Command == "install" || result.Caddy.Command == "uninstall") && caddyPending.CompareAndSwap(false, true) {
		action := result.Caddy.Command
		go func() {
			defer caddyPending.Store(false)
			actionCtx, done := context.WithTimeout(ctx, 15*time.Minute)
			defer done()
			callErr := local.Call(actionCtx, "POST", "/caddy", map[string]string{"action": action}, nil)
			if callErr != nil {
				log.Printf("Caddy API operation failed: %v", callErr)
			}
		}()
	}

	return nil
}
