package payincus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ResetExtension signals support for wiping node-local state from the terminal.
const ResetExtension = "payincus_node_reset_v1"

// ResetSummary reports what the wipe removed so the operator can verify it.
type ResetSummary struct {
	Instances    []string `json:"instances"`
	Volumes      []string `json:"volumes"`
	StoragePools []string `json:"storagePools"`
	Networks     []string `json:"networks"`
	Profiles     []string `json:"profiles"`
	Projects     []string `json:"projects"`
	ImagesKept   int      `json:"imagesKept"`
}

// resetLocalClient is deliberately narrow: it reuses the daemon's own unix
// socket so the wipe goes through the same validated API as the CLI.
type resetLocalClient interface {
	call(ctx context.Context, method, path string, input any) (json.RawMessage, error)
}

// unixSocketClient talks to this daemon over its own unix socket. The daemon
// answers converted requests as trusted local access, so the wipe path exercises
// exactly the same validation as the incus CLI instead of reaching into the
// database directly.
type unixSocketClient struct{}

func (unixSocketClient) call(ctx context.Context, method, path string, input any) (json.RawMessage, error) {
	root := os.Getenv("INCUS_DIR")
	if root == "" {
		root = "/var/lib/incus"
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", filepath.Join(root, "unix.socket"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(payload)
	}

	// The unix socket speaks plain HTTP; HTTPS is only for the network listener.
	req, err := http.NewRequestWithContext(ctx, method, "http://incus"+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Type       string          `json:"type"`
		StatusCode int             `json:"status_code"`
		Error      string          `json:"error"`
		Metadata   json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, fmt.Errorf("unexpected API response (%d)", resp.StatusCode)
	}
	if resp.StatusCode >= 400 || envelope.Error != "" {
		message := envelope.Error
		if message == "" {
			message = fmt.Sprintf("API returned %d", resp.StatusCode)
		}
		return nil, errors.New(message)
	}
	if len(envelope.Metadata) == 0 || string(envelope.Metadata) == "null" {
		return json.RawMessage("null"), nil
	}
	return envelope.Metadata, nil
}

// HandleReset wipes everything a previous panel left behind so a reinstalled
// control plane starts from a clean node. Image cache is intentionally kept:
// re-downloading images is slow and carries no stale control-plane identity.
// Only a local unix-socket caller may run this; remote mTLS clients cannot.
func (s *Service) HandleReset(w http.ResponseWriter, r *http.Request, client resetLocalClient) {
	if r.Method != http.MethodPost {
		respond(w, 405, "method not allowed")
		return
	}
	if r.TLS != nil {
		respond(w, 403, "node reset is local terminal only")
		return
	}

	select {
	case s.caddyGate <- struct{}{}:
		defer func() { <-s.caddyGate }()
	default:
		respond(w, 409, "busy")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	summary, err := resetNodeState(ctx, client)
	if err != nil {
		log.Printf("[payincus] node reset incomplete: %v", err)
		respond(w, 500, "node reset incomplete; inspect incusd journal")
		return
	}

	log.Printf("[payincus] node reset complete: instances=%d volumes=%d pools=%d networks=%d profiles=%d projects=%d imagesKept=%d",
		len(summary.Instances), len(summary.Volumes), len(summary.StoragePools), len(summary.Networks), len(summary.Profiles), len(summary.Projects), summary.ImagesKept)
	respond(w, 200, summary)
}

// detachDefaultProfileDevices removes the storage and network devices a wiped
// pool or bridge would otherwise still be referenced by. The profile keeps its
// identity and config so a reinstalled panel can re-add devices cleanly.
func detachDefaultProfileDevices(ctx context.Context, client resetLocalClient) error {
	body, err := client.call(ctx, http.MethodGet, "/1.0/profiles/default", nil)
	if err != nil {
		return fmt.Errorf("read default profile: %w", err)
	}
	var profile struct {
		Config      map[string]string            `json:"config"`
		Description string                       `json:"description"`
		Devices     map[string]map[string]string `json:"devices"`
	}
	if json.Unmarshal(body, &profile) != nil {
		return errors.New("invalid default profile response")
	}
	if len(profile.Devices) == 0 {
		return nil
	}
	// Incus has no per-device delete endpoint for profiles, and PATCH treats an
	// empty device map as "keep existing". Only a full PUT replaces the map, so
	// send the reduced device set together with the untouched config.
	remaining := map[string]map[string]string{}
	changed := false
	for name, device := range profile.Devices {
		if device["type"] == "disk" || device["type"] == "nic" {
			changed = true
			continue
		}
		remaining[name] = device
	}
	if !changed {
		return nil
	}
	payload := map[string]any{"config": profile.Config, "description": profile.Description, "devices": remaining}
	_, err = client.call(ctx, http.MethodPut, "/1.0/profiles/default", payload)
	if err != nil {
		return fmt.Errorf("detach default profile devices: %w", err)
	}
	return nil
}

// resetNodeState removes instances, volumes, pools that hold instance data,
// managed networks, non-default profiles and non-default projects. It never
// touches remote images or the default project/profile scaffolding itself.
func resetNodeState(ctx context.Context, client resetLocalClient) (ResetSummary, error) {
	summary := ResetSummary{}
	list := func(path string) ([]string, error) {
		var names []string
		body, err := client.call(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if json.Unmarshal(body, &names) != nil {
			return nil, fmt.Errorf("invalid response for %s", path)
		}
		sort.Strings(names)
		return names, nil
	}

	// Stop and delete every instance across projects first: volumes and pools
	// refuse deletion while they are still in use.
	instances, err := list("/1.0/instances?all-projects=true&recursion=0")
	if err != nil {
		return summary, err
	}
	var failures []error
	for _, path := range instances {
		name := strings.TrimPrefix(path, "/1.0/instances/")
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		_, _ = client.call(ctx, http.MethodPut, "/1.0/instances/"+name+"/state", map[string]any{"action": "stop", "force": true, "timeout": -1})
		if _, delErr := client.call(ctx, http.MethodDelete, "/1.0/instances/"+name+"?force=true", nil); delErr != nil {
			failures = append(failures, fmt.Errorf("delete instance %s: %w", name, delErr))
			continue
		}
		summary.Instances = append(summary.Instances, name)
	}

	images, imagesErr := list("/1.0/images?recursion=0")
	if imagesErr == nil {
		summary.ImagesKept = len(images)
	}

	// Pools and networks referenced by the default profile cannot be deleted.
	// Detach every device first; the profile object itself stays.
	if detachErr := detachDefaultProfileDevices(ctx, client); detachErr != nil {
		failures = append(failures, detachErr)
	}

	// Remove custom volumes before their pools, otherwise the pool is still used.
	pools, poolsErr := list("/1.0/storage-pools?recursion=0")
	if poolsErr == nil {
		for _, poolPath := range pools {
			poolName := strings.TrimPrefix(poolPath, "/1.0/storage-pools/")
			if poolName == "" || strings.Contains(poolName, "/") {
				continue
			}
			volumes, volErr := list("/1.0/storage-pools/" + poolName + "/volumes/custom?recursion=0")
			if volErr == nil {
				for _, volPath := range volumes {
					volName := strings.TrimPrefix(volPath, "/1.0/storage-pools/"+poolName+"/volumes/custom/")
					if volName == "" || strings.Contains(volName, "/") {
						continue
					}
					if _, delErr := client.call(ctx, http.MethodDelete, "/1.0/storage-pools/"+poolName+"/volumes/custom/"+volName, nil); delErr != nil {
						failures = append(failures, fmt.Errorf("delete volume %s/%s: %w", poolName, volName, delErr))
						continue
					}
					summary.Volumes = append(summary.Volumes, poolName+"/"+volName)
				}
			}
			// Instance images stored per pool go with the pool; keep only remote cache.
			if _, delErr := client.call(ctx, http.MethodDelete, "/1.0/storage-pools/"+poolName, nil); delErr != nil {
				failures = append(failures, fmt.Errorf("delete pool %s: %w", poolName, delErr))
				continue
			}
			summary.StoragePools = append(summary.StoragePools, poolName)
		}
	}

	networks, networksErr := list("/1.0/networks?recursion=0")
	if networksErr == nil {
		for _, networkPath := range networks {
			name := strings.TrimPrefix(networkPath, "/1.0/networks/")
			if name == "" || strings.Contains(name, "/") {
				continue
			}
			var detail struct {
				Managed bool   `json:"managed"`
				Type    string `json:"type"`
			}
			body, getErr := client.call(ctx, http.MethodGet, "/1.0/networks/"+name, nil)
			if getErr != nil || json.Unmarshal(body, &detail) != nil || !detail.Managed || detail.Type != "bridge" {
				continue
			}
			if _, delErr := client.call(ctx, http.MethodDelete, "/1.0/networks/"+name, nil); delErr != nil {
				failures = append(failures, fmt.Errorf("delete network %s: %w", name, delErr))
				continue
			}
			summary.Networks = append(summary.Networks, name)
		}
	}

	profiles, profilesErr := list("/1.0/profiles?recursion=0")
	if profilesErr == nil {
		for _, profilePath := range profiles {
			name := strings.TrimPrefix(profilePath, "/1.0/profiles/")
			if name == "" || name == "default" || strings.Contains(name, "/") {
				continue
			}
			if _, delErr := client.call(ctx, http.MethodDelete, "/1.0/profiles/"+name, nil); delErr != nil {
				failures = append(failures, fmt.Errorf("delete profile %s: %w", name, delErr))
				continue
			}
			summary.Profiles = append(summary.Profiles, name)
		}
	}

	projects, projectsErr := list("/1.0/projects?recursion=0")
	if projectsErr == nil {
		for _, projectPath := range projects {
			name := strings.TrimPrefix(projectPath, "/1.0/projects/")
			if name == "" || name == "default" || strings.Contains(name, "/") {
				continue
			}
			if _, delErr := client.call(ctx, http.MethodDelete, "/1.0/projects/"+name, nil); delErr != nil {
				failures = append(failures, fmt.Errorf("delete project %s: %w", name, delErr))
				continue
			}
			summary.Projects = append(summary.Projects, name)
		}
	}

	return summary, errors.Join(failures...)
}
