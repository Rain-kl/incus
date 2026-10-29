package report

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	nodesecurity "github.com/lxc/incus/v7/internal/server/payincus/security"
)

// Storage telemetry is collected locally with one deadline, never by panel read requests.
func collectStorageReport() map[string]any {
	result := map[string]any{"available": false, "reportedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	socket, ok := detectIncusSocket()
	if !ok {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := newIncusUnixHTTPClient(socket)
	return storageReport(ctx, client, result)
}

// LockedStoragePayload reports recovery identities without instance or host-resource inventory.
func LockedStoragePayload() map[string]any {
	return map[string]any{"storage": collectStorageReport()}
}

type reportedPool struct {
	Name        string            `json:"name"`
	Driver      string            `json:"driver"`
	Status      string            `json:"status"`
	Description string            `json:"description"`
	Config      map[string]string `json:"config"`
	UsedBy      []string          `json:"used_by"`
}

type encryptionDataset struct {
	Dataset        string `json:"dataset"`
	EncryptionRoot string `json:"encryptionRoot"`
	Algorithm      string `json:"algorithm"`
	KeyStatus      string `json:"keyStatus"`
	KeyFormat      string `json:"keyFormat"`
}

type storageInventory struct {
	Locked   bool                `json:"locked"`
	Datasets []encryptionDataset `json:"datasets"`
	Locks    map[string]struct {
		Locked          bool  `json:"locked"`
		Complete        bool  `json:"complete"`
		ResumeInstances []any `json:"resumeInstances"`
	} `json:"locks"`
}

var inspectZFSStorage = func(ctx context.Context, parent string) (map[string]any, bool) {
	storage := &nodesecurity.Storage{Run: nodesecurity.Run}
	health, err := storage.Run(ctx, "zpool", []string{"list", "-H", "-o", "health", parent}, nil)
	if err != nil || strings.TrimSpace(string(health)) != "ONLINE" {
		return nil, false
	}
	backing, err := storage.LoopBacking(ctx, parent)
	if err != nil {
		return nil, errors.Is(err, nodesecurity.ErrNonFilePool)
	}
	info, err := os.Stat(backing)
	var stat syscall.Statfs_t
	if err != nil || syscall.Statfs(backing, &stat) != nil {
		return nil, false
	}
	return map[string]any{"sizeBytes": info.Size(), "id": fmt.Sprint(stat.Fsid), "availableBytes": stat.Bavail * uint64(stat.Bsize)}, true
}

func storageReport(ctx context.Context, client *http.Client, result map[string]any) map[string]any {
	pools, err := incusRequestContext[[]reportedPool](ctx, client, "/1.0/storage-pools?recursion=1")
	if err != nil {
		return result
	}
	result["available"] = true
	result["complete"] = len(pools) <= 256
	if len(pools) > 256 {
		pools = pools[:256]
	}
	var inventory storageInventory
	// Include external/pending encrypted datasets even when Incus has no pools bound yet.
	inventoryCtx, inventoryCancel := context.WithTimeout(ctx, 2*time.Second)
	inventory, err = incusRequestContext[storageInventory](inventoryCtx, client, "/1.0/payincus/storage")
	inventoryCancel()
	encryptionKnown := err == nil
	if encryptionKnown {
		result["datasets"] = inventory.Datasets
		result["locks"] = inventory.Locks
	}
	items := make([]map[string]any, len(pools))
	var wg sync.WaitGroup
	gate := make(chan struct{}, 4)
	for index, pool := range pools {
		wg.Add(1)
		go func(index int, pool reportedPool) {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			item := map[string]any{"name": pool.Name, "driver": pool.Driver, "config": pool.Config, "status": "unavailable", "usedBy": len(pool.UsedBy)}
			items[index] = item
			if (pool.Driver == "dir" || pool.Driver == "btrfs") && filepath.Base(pool.Name) == pool.Name && len(incusSocketCandidates) > 0 {
				path := filepath.Join(filepath.Dir(incusSocketCandidates[0]), "storage-pools", pool.Name)
				var stat syscall.Statfs_t
				if syscall.Statfs(path, &stat) == nil {
					item["filesystemId"] = fmt.Sprint(stat.Fsid)
				}
			}
			if pool.Driver == "zfs" {
				item["encryptionKnown"] = encryptionKnown
				source := pool.Config["zfs.pool_name"]
				if source == "" {
					source = pool.Config["source"]
				}
				for _, dataset := range inventory.Datasets {
					if dataset.Dataset != source {
						continue
					}
					encrypted := dataset.Algorithm != "off" && dataset.Algorithm != "-"
					locked := inventory.Locked || inventory.Locks[dataset.EncryptionRoot].Locked || (encrypted && dataset.KeyStatus != "available")
					lock := inventory.Locks[dataset.EncryptionRoot]
					item["encryption"] = map[string]any{"encrypted": encrypted, "nodeLocked": inventory.Locked, "source": source, "encryptionRoot": dataset.EncryptionRoot, "algorithm": dataset.Algorithm, "keyFormat": dataset.KeyFormat, "state": "unlocked", "manualLocked": lock.Locked, "lockPending": lock.Locked && !lock.Complete, "pendingRecovery": len(lock.ResumeInstances)}
					if locked {
						item["status"] = "locked"
						if encrypted && dataset.KeyStatus != "available" {
							item["encryption"].(map[string]any)["state"] = "locked"
						}
						return
					}
				}
				if !encryptionKnown || item["encryption"] == nil {
					return
				}
				parent := strings.Split(source, "/")[0]
				if nodesecurity.ValidEncryptionRoot(parent) {
					backing, healthy := inspectZFSStorage(ctx, parent)
					if !healthy {
						return
					}
					if backing != nil {
						item["backing"] = backing
					}
				}
			}
			if inventory.Locked {
				item["status"] = "locked"
				return
			}
			if pool.Status != "" && !strings.EqualFold(pool.Status, "Created") {
				return
			}
			resources, err := incusRequestContext[map[string]any](ctx, client, "/1.0/storage-pools/"+url.PathEscape(pool.Name)+"/resources")
			if err == nil {
				item["space"] = resources["space"]
				item["status"] = "ready"
			}
		}(index, pool)
	}
	wg.Wait()
	result["pools"] = items
	complete, ok := result["complete"].(bool)
	complete = ok && complete && encryptionKnown
	for _, item := range items {
		if item["status"] != "ready" && item["status"] != "locked" {
			complete = false
		}
	}
	result["collectionComplete"] = complete
	return result
}
