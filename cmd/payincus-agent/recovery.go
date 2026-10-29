package main

import (
	"context"
	"log"
	"time"

	"github.com/lxc/incus/v7/internal/server/payincus/nodeclient"
	"github.com/lxc/incus/v7/internal/server/payincus/panel"
)

// startupRecovery remains independent of heartbeat and never asks for keys while locally locked.
func startupRecovery(ctx context.Context, local *nodeclient.Client, remote *panel.Client) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		var state struct {
			Locked            bool `json:"locked"`
			RecoveryPending   bool `json:"recoveryPending"`
			StartupRecovering bool `json:"startupRecovering"`
		}
		err := local.Call(attemptCtx, "GET", "/security/startup", nil, &state)
		if err == nil && !state.Locked && (state.RecoveryPending || state.StartupRecovering) {
			var grant panel.RecoveryGrant
			grant, err = remote.StartupKeys(attemptCtx)
			if err == nil {
				err = local.Call(attemptCtx, "POST", "/security/startup", map[string]any{"keys": grant.Keys, "locked": grant.Locked, "generation": grant.Generation}, nil)
				for i := range grant.Keys {
					grant.Keys[i].Key = ""
				}
			}
			if err != nil {
				log.Print("Node startup recovery incomplete; retrying")
			}
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
