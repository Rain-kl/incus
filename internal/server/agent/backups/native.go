package backups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/internal/server/agent/localapi"
	"github.com/lxc/incus/v7/shared/api"
)

type nativeSource struct {
	connect func(context.Context) (incus.InstanceServer, error)
}

func (n nativeSource) client(ctx context.Context) (incus.InstanceServer, error) {
	if n.connect != nil {
		return n.connect(ctx)
	}
	return nativeClient(ctx)
}

func nativeClient(ctx context.Context) (incus.InstanceServer, error) {
	return incus.ConnectIncusUnixWithContext(ctx, localapi.SocketCandidates()[0], nil)
}

func (n nativeSource) Inventory(ctx context.Context, destination string) ([]Item, error) {
	client, err := n.client(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect()
	server, _, err := client.GetServer()
	if err != nil {
		return nil, err
	}
	instances, err := client.GetInstancesAllProjects(api.InstanceTypeAny)
	if err != nil {
		return nil, err
	}
	pools, err := client.GetStoragePools()
	if err != nil {
		return nil, err
	}
	// Avoid putting an archive inside a volume that the same task will read.
	for _, pool := range pools {
		source := pool.Config["source"]
		if filepath.IsAbs(source) {
			resolved, resolveErr := filepath.EvalSymlinks(source)
			if resolveErr == nil && (destination == resolved || strings.HasPrefix(destination, resolved+string(os.PathSeparator))) {
				return nil, errors.New("backup directory is inside an Incus storage source")
			}
		}
	}
	items := []Item{}
	for _, instance := range instances {
		if server.Environment.ServerClustered && instance.Location != "" && instance.Location != server.Environment.ServerName {
			continue
		}
		for _, device := range instance.ExpandedDevices {
			if device["type"] == "disk" && filepath.IsAbs(device["source"]) {
				resolved, resolveErr := filepath.EvalSymlinks(device["source"])
				if resolveErr == nil && (destination == resolved || strings.HasPrefix(destination, resolved+string(os.PathSeparator))) {
					return nil, fmt.Errorf("backup directory is inside instance %s/%s disk source", instance.Project, instance.Name)
				}
			}
		}
		items = append(items, Item{Project: instance.Project, Name: instance.Name, Status: "pending"})
	}
	return items, nil
}

func (n nativeSource) Export(ctx context.Context, item Item, writer io.WriteSeeker) (result error) {
	client, err := n.client(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect()
	client = client.UseProject(item.Project)
	req := api.InstanceBackupsPost{InstanceOnly: true, CompressionAlgorithm: "gzip"}
	file := &incus.BackupFileRequest{BackupFile: writer}
	if client.HasExtension("direct_backup") {
		return client.CreateInstanceBackupStream(item.Name, req, file)
	}
	req.Name = "node-" + randomName()
	reportProgress(ctx, fmt.Sprintf("Exporting %s: creating temporary backup", item.Name))
	operation, err := client.CreateInstanceBackup(item.Name, req)
	if err != nil {
		return err
	}
	attachOperationProgress(ctx, operation, "Exporting "+item.Name)
	if err = operation.WaitContext(ctx); err != nil {
		return err
	}
	defer func() {
		cleanup, cleanupErr := client.DeleteInstanceBackup(item.Name, req.Name)
		if cleanupErr == nil {
			cleanupErr = cleanup.Wait()
		}
		if cleanupErr != nil {
			result = errors.Join(result, fmt.Errorf("temporary backup cleanup failed: %w", cleanupErr))
		}
	}()
	reportProgress(ctx, fmt.Sprintf("Exporting %s: downloading archive", item.Name))
	_, err = client.GetInstanceBackupFile(item.Name, req.Name, file)
	return err
}

func (n nativeSource) Import(ctx context.Context, reader io.Reader, name string, poolName string) (string, error) {
	client, err := n.client(ctx)
	if err != nil {
		return "", err
	}
	defer client.Disconnect()
	resolvedPool, err := resolveImportPool(client, poolName)
	if err != nil {
		return "", err
	}
	op, err := client.CreateInstanceFromBackup(incus.InstanceBackupArgs{
		BackupFile: reader,
		Name:       name,
		PoolName:   resolvedPool,
	})
	if err != nil {
		return "", err
	}
	reportProgress(ctx, "Importing: unpacking instance on host")
	attachOperationProgress(ctx, op, "Importing")
	if err = op.WaitContext(ctx); err != nil {
		return "", err
	}
	importedName := resolveImportedInstanceName(name, op.Get())
	if importedName != "" {
		removed := sanitizeImportedHostDevices(ctx, client, importedName)
		if len(removed) > 0 {
			reportProgress(ctx, fmt.Sprintf("Importing: removed incompatible host GPU devices (%s)", strings.Join(removed, ", ")))
		}
	}
	return importedName, nil
}

func resolveImportedInstanceName(requested string, opAPI api.Operation) string {
	if requested != "" {
		return requested
	}
	if opAPI.Resources != nil {
		for _, ref := range opAPI.Resources["instances"] {
			base := path.Base(strings.TrimSuffix(ref, "/"))
			if base != "" && base != "." && base != "/" {
				return base
			}
		}
	}
	return ""
}

func sanitizeImportedHostDevices(ctx context.Context, client incus.InstanceServer, instanceName string) []string {
	inst, etag, err := client.GetInstance(instanceName)
	if err != nil || inst == nil {
		return nil
	}
	hasGPUDevice := false
	for _, dev := range inst.Devices {
		if dev["type"] == "gpu" {
			hasGPUDevice = true
			break
		}
	}
	hasNvidiaConfig := inst.Config["nvidia.runtime"] != "" || inst.Config["nvidia.driver.capabilities"] != ""
	if !hasGPUDevice && !hasNvidiaConfig {
		return nil
	}
	resources, err := client.GetServerResources()
	if err != nil || resources == nil {
		return nil
	}
	hostPCIs := make(map[string]struct{}, len(resources.GPU.Cards))
	for _, card := range resources.GPU.Cards {
		pci := strings.ToLower(strings.TrimSpace(card.PCIAddress))
		if pci != "" {
			hostPCIs[pci] = struct{}{}
		}
	}

	writable := inst.Writable()
	devices := make(map[string]map[string]string, len(writable.Devices))
	devNames := make([]string, 0, len(writable.Devices))
	for devName := range writable.Devices {
		devNames = append(devNames, devName)
	}
	slices.Sort(devNames)

	var removed []string
	remainingGPUs := 0
	for _, devName := range devNames {
		dev := writable.Devices[devName]
		if dev["type"] == "gpu" {
			pci := strings.ToLower(strings.TrimSpace(dev["pci"]))
			_, existsOnHost := hostPCIs[pci]
			if (pci != "" && !existsOnHost) || (pci == "" && len(hostPCIs) == 0) {
				rawPCI := strings.TrimSpace(dev["pci"])
				if rawPCI != "" {
					removed = append(removed, fmt.Sprintf("%s(%s)", devName, rawPCI))
				} else {
					removed = append(removed, devName)
				}
				continue
			}
			remainingGPUs++
		}
		cloned := make(map[string]string, len(dev))
		for k, v := range dev {
			cloned[k] = v
		}
		devices[devName] = cloned
	}

	config := make(map[string]string, len(writable.Config))
	for k, v := range writable.Config {
		config[k] = v
	}
	configChanged := false
	if remainingGPUs == 0 {
		for _, key := range []string{"nvidia.runtime", "nvidia.driver.capabilities"} {
			if _, ok := config[key]; ok {
				delete(config, key)
				configChanged = true
			}
		}
	}
	if len(removed) == 0 && !configChanged {
		return nil
	}
	writable.Devices = devices
	writable.Config = config
	op, err := client.UpdateInstance(instanceName, writable, etag)
	if err != nil {
		return nil
	}
	if op != nil && op.Get().ID != "" {
		err = op.WaitContext(ctx)
		if err != nil {
			return nil
		}
	}
	return removed
}

func attachOperationProgress(ctx context.Context, op incus.Operation, prefix string) {
	if op == nil || op.Get().StatusCode.IsFinal() {
		return
	}
	_, _ = op.AddHandler(func(opAPI api.Operation) {
		for _, key := range []string{"progress", "create_instance_from_backup_progress"} {
			raw, ok := opAPI.Metadata[key]
			if !ok {
				continue
			}
			switch val := raw.(type) {
			case string:
				if strings.TrimSpace(val) != "" {
					reportProgress(ctx, fmt.Sprintf("%s: %s", prefix, strings.TrimSpace(val)))
					return
				}
			case map[string]any:
				if stage, ok := val["stage"].(string); ok && strings.TrimSpace(stage) != "" {
					if pct, ok := val["percent"].(float64); ok && pct > 0 {
						reportProgress(ctx, fmt.Sprintf("%s: %s (%d%%)", prefix, strings.TrimSpace(stage), int(pct)))
					} else {
						reportProgress(ctx, fmt.Sprintf("%s: %s", prefix, strings.TrimSpace(stage)))
					}
					return
				}
			}
		}
	})
}

func resolveImportPool(client incus.InstanceServer, requested string) (string, error) {
	poolNames, poolsErr := client.GetStoragePoolNames()
	if requested != "" {
		if poolsErr == nil && !slices.Contains(poolNames, requested) {
			return "", fmt.Errorf("target storage pool %q does not exist", requested)
		}
		return requested, nil
	}
	profile, _, profErr := client.GetProfile("default")
	if profErr == nil && !hasRootDiskDevice(profile.Devices) && poolsErr == nil {
		if len(poolNames) == 0 {
			return "", errors.New("no storage pool configured on target node")
		}
		if slices.Contains(poolNames, "default") {
			return "default", nil
		}
		sorted := slices.Clone(poolNames)
		slices.Sort(sorted)
		return sorted[0], nil
	}
	return "", nil
}

func hasRootDiskDevice(devices map[string]map[string]string) bool {
	for _, dev := range devices {
		if dev["type"] == "disk" && dev["path"] == "/" {
			return true
		}
	}
	return false
}
