// payincus-agent carries authenticated connections; node operations live in incusd.
package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	refresh := make(chan struct{}, 1)
	worker := tunnel.NewWorker(tunnel.WorkerConfig{PanelURL: cfg.PanelURL, AgentID: cfg.AgentID, AgentSecret: cfg.AgentSecret, NodeClient: local, RequestHeartbeat: func() {
		select {
		case refresh <- struct{}{}:
		default:
		}
	}})
	defer worker.SyncConfig(false, "", 0)
	go securityReports(ctx, local, remote)
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
		case <-refresh:
		}
	}
}

func heartbeat(ctx context.Context, local *nodeclient.Client, remote *panel.Client, cfg config.Config, worker *tunnel.Worker, caddyPending *atomic.Bool) error {
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var payload map[string]any
	localCtx, localCancel := context.WithTimeout(requestCtx, 45*time.Second)
	localErr := local.Check(localCtx)
	if localErr == nil {
		localErr = local.Call(localCtx, "GET", fmt.Sprintf("/report?interval=%d", cfg.HeartbeatIntervalSeconds), nil, &payload)
	}
	localCancel()
	if payload == nil && localErr == nil {
		localErr = fmt.Errorf("node report unavailable")
	}
	if localErr != nil || payload == nil {
		// Keep transport reporting alive when incusd cannot collect telemetry.
		payload = map[string]any{
			"incus": map[string]any{"available": false}, "instances": map[string]any{"available": false},
			"storage":   map[string]any{"available": false, "reportedAt": time.Now().UTC().Format(time.RFC3339Nano)},
			"resources": map[string]any{"storageAvailable": false},
			"metrics":   map[string]any{"heartbeatIntervalSeconds": cfg.HeartbeatIntervalSeconds},
		}
	}

	payload["version"] = version
	payload["nodeApi"] = "payincus_node_v1"
	capabilities, ok := payload["capabilities"].([]any)
	if !ok {
		capabilities = nil
	}
	payload["capabilities"] = append(capabilities, "network-control", "payincus_node_v1")
	result, err := remote.Heartbeat(requestCtx, payload)
	if err != nil {
		return err
	}

	if result.Tunnel != nil {
		worker.SyncConfig(result.Tunnel.Enabled, result.Tunnel.TargetHost, result.Tunnel.TargetPort)
	}

	if localErr == nil && result.Caddy != nil && (result.Caddy.Command == "install" || result.Caddy.Command == "uninstall") && caddyPending.CompareAndSwap(false, true) {
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

	return localErr
}

// securityReports uses a separate bounded cadence so alarms are not delayed by resource collection.
func securityReports(ctx context.Context, local *nodeclient.Client, remote *panel.Client) {
	var lastPayload []byte
	var deliveredAt time.Time
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var report map[string]any
		err := local.Call(callCtx, "GET", "/security/report", nil, &report)
		if err == nil {
			payload, marshalErr := json.Marshal(report)
			if marshalErr == nil && bytes.Equal(payload, lastPayload) && time.Since(deliveredAt) < 30*time.Second {
				cancel()
				continue
			}
			var ids []string
			ids, err = remote.SecurityReport(callCtx, report)
			if err == nil {
				lastPayload = payload
				deliveredAt = time.Now()
			}
			if err == nil && len(ids) > 0 {
				err = local.Call(callCtx, "POST", "/security/ack", map[string]any{"ids": ids}, nil)
			}
		}
		cancel()
		if err != nil {
			log.Print("Node report delivery failed")
		}
	}
}
