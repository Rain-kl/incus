package main

import (
	"context"
	"errors"
	"strings"

	"github.com/lxc/incus/v7/internal/server/instance"
	"github.com/lxc/incus/v7/internal/server/instance/instancetype"
	nodesecurity "github.com/lxc/incus/v7/internal/server/payincus/security"
	storagePools "github.com/lxc/incus/v7/internal/server/storage"
)

// PayIncus: resolve expanded disk devices through native pools before affecting any workload.
func payIncusStorageInstances(ctx context.Context, d *Daemon, dataset string) ([]instance.Instance, error) {
	items, err := instance.LoadNodeAll(d.State(), instancetype.Any)
	if err != nil {
		return nil, err
	}
	selected := []instance.Instance{}
	pools := map[string]bool{}
	for _, inst := range items {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !inst.IsRunning() {
			continue
		}
		related := false
		for _, dev := range inst.ExpandedDevices() {
			if dev["type"] != "disk" || dev["pool"] == "" {
				continue
			}
			name := dev["pool"]
			match, known := pools[name]
			if !known {
				pool, err := storagePools.LoadByName(d.State(), name)
				if err != nil {
					return nil, err
				}
				info := pool.ToAPI()
				source := info.Config["zfs.pool_name"]
				if source == "" {
					source = info.Config["source"]
				}
				match = info.Driver == "zfs" && (source == dataset || strings.HasPrefix(source, dataset+"/"))
				pools[name] = match
			}
			related = related || match
		}
		if related {
			selected = append(selected, inst)
		}
	}
	return selected, nil
}

func installPayIncusStorageWorkloads(d *Daemon) {
	nodesecurity.Default().SetPoolWorkloads(func(ctx context.Context, dataset string) ([]nodesecurity.ResumeInstance, error) {
		items, err := payIncusStorageInstances(ctx, d, dataset)
		if err != nil {
			return nil, err
		}
		pending := []nodesecurity.ResumeInstance{}
		for _, inst := range items {
			pending = append(pending, nodesecurity.ResumeInstance{Project: inst.Project().Name, Name: inst.Name(), UUID: inst.LocalConfig()["volatile.uuid"], Frozen: inst.IsFrozen()})
		}
		return pending, nil
	}, func(ctx context.Context, dataset string) error {
		items, err := payIncusStorageInstances(ctx, d, dataset)
		if err != nil {
			return err
		}
		for _, inst := range items {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if inst.Stop(false) != nil {
				return errors.New("related workload stop failed")
			}
		}
		remaining, err := payIncusStorageInstances(ctx, d, dataset)
		if err != nil || len(remaining) != 0 {
			return errors.New("related workloads remain running")
		}
		return nil
	})
}
