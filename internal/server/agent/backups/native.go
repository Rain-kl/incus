package backups

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
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
	op, err := client.CreateInstanceFromBackup(incus.InstanceBackupArgs{
		BackupFile: reader,
		Name:       name,
		PoolName:   poolName,
	})
	if err != nil {
		return "", err
	}
	reportProgress(ctx, "Importing: unpacking instance on host")
	attachOperationProgress(ctx, op, "Importing")
	if err = op.WaitContext(ctx); err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	opAPI := op.Get()
	if opAPI.Resources != nil {
		for _, ref := range opAPI.Resources["instances"] {
			base := path.Base(strings.TrimSuffix(ref, "/"))
			if base != "" && base != "." && base != "/" {
				return base, nil
			}
		}
	}
	return "", nil
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
