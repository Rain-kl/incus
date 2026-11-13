package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeResetClient records actions and answers list requests from a fixed
// inventory, so the wipe order can be asserted without a live daemon.
type fakeResetClient struct {
	instances []string
	volumes   map[string][]string
	pools     []string
	networks  []string
	profiles  []string
	projects  []string
	images    []string
	profile   map[string]any
	failOn    string
	actions   []string
}

func (f *fakeResetClient) call(_ context.Context, method, path string, input any) (json.RawMessage, error) {
	f.actions = append(f.actions, method+" "+path)
	if f.failOn != "" && strings.Contains(method+" "+path, f.failOn) {
		return nil, errors.New("injected failure")
	}
	list := func(items []string) (json.RawMessage, error) { return json.Marshal(items) }
	switch {
	case method == http.MethodGet && path == "/1.0/instances?all-projects=true&recursion=0":
		return list(f.instances)
	case method == http.MethodGet && path == "/1.0/images?recursion=0":
		return list(f.images)
	case method == http.MethodGet && path == "/1.0/storage-pools?recursion=0":
		return list(f.pools)
	case method == http.MethodGet && path == "/1.0/networks?recursion=0":
		return list(f.networks)
	case method == http.MethodGet && path == "/1.0/profiles?recursion=0":
		return list(f.profiles)
	case method == http.MethodGet && path == "/1.0/projects?recursion=0":
		return list(f.projects)
	case method == http.MethodGet && path == "/1.0/profiles/default":
		return json.Marshal(f.profile)
	case method == http.MethodGet && strings.HasPrefix(path, "/1.0/networks/"):
		return json.Marshal(map[string]any{"managed": true, "type": "bridge"})
	case method == http.MethodGet && strings.Contains(path, "/volumes/custom?recursion=0"):
		pool := strings.TrimSuffix(strings.TrimPrefix(path, "/1.0/storage-pools/"), "/volumes/custom?recursion=0")
		return list(f.volumes[pool])
	}
	return json.RawMessage("null"), nil
}

func (f *fakeResetClient) has(prefix string) bool {
	for _, action := range f.actions {
		if strings.HasPrefix(action, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeResetClient) indexOf(prefix string) int {
	for i, action := range f.actions {
		if strings.HasPrefix(action, prefix) {
			return i
		}
	}
	return -1
}

func newResetService() *Service {
	return &Service{caddyGate: make(chan struct{}, 1)}
}

// A wiped node must only be triggerable from the local terminal.
func TestHandleResetRejectsRemoteAndNonPost(t *testing.T) {
	service := newResetService()
	request := httptest.NewRequest(http.MethodPost, "/1.0/agent/reset", nil)
	request.TLS = &tls.ConnectionState{}
	recorder := httptest.NewRecorder()
	service.HandleReset(recorder, request, &fakeResetClient{})
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("remote reset must be forbidden, got %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	service.HandleReset(recorder, httptest.NewRequest(http.MethodGet, "/1.0/agent/reset", nil), &fakeResetClient{})
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("reset must reject non-POST, got %d", recorder.Code)
	}
}

// Busy gate protects concurrent maintenance operations.
func TestHandleResetRefusesWhenBusy(t *testing.T) {
	service := newResetService()
	service.caddyGate <- struct{}{}
	recorder := httptest.NewRecorder()
	service.HandleReset(recorder, httptest.NewRequest(http.MethodPost, "/1.0/agent/reset", nil), &fakeResetClient{})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("concurrent reset must be refused, got %d", recorder.Code)
	}
}

// The wipe must delete dependants before their parents or Incus refuses the
// delete, and it must detach the default profile before dropping its pool.
func TestResetNodeStateOrdersDeletesAndDetachesProfile(t *testing.T) {
	client := &fakeResetClient{
		instances: []string{"/1.0/instances/web"},
		pools:     []string{"/1.0/storage-pools/syspool"},
		volumes:   map[string][]string{"syspool": {"/1.0/storage-pools/syspool/volumes/custom/data"}},
		networks:  []string{"/1.0/networks/incusbr0"},
		profiles:  []string{"/1.0/profiles/default", "/1.0/profiles/extra"},
		projects:  []string{"/1.0/projects/default", "/1.0/projects/extra"},
		images:    []string{"/1.0/images/abc"},
		profile:   map[string]any{"config": map[string]string{}, "description": "x", "devices": map[string]any{"root": map[string]string{"type": "disk", "pool": "syspool"}, "eth0": map[string]string{"type": "nic", "network": "incusbr0"}}},
	}
	summary, err := resetNodeState(context.Background(), client)
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	if strings.Join(summary.Instances, ",") != "web" || strings.Join(summary.StoragePools, ",") != "syspool" ||
		strings.Join(summary.Networks, ",") != "incusbr0" || strings.Join(summary.Profiles, ",") != "extra" ||
		strings.Join(summary.Projects, ",") != "extra" || strings.Join(summary.Volumes, ",") != "syspool/data" {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if summary.ImagesKept != 1 {
		t.Fatalf("image cache must be preserved, got %d", summary.ImagesKept)
	}

	detach := client.indexOf("PUT /1.0/profiles/default")
	poolDelete := client.indexOf("DELETE /1.0/storage-pools/syspool")
	instanceDelete := client.indexOf("DELETE /1.0/instances/web")
	volumeDelete := client.indexOf("DELETE /1.0/storage-pools/syspool/volumes/custom/data")
	if detach < 0 || poolDelete < 0 || instanceDelete < 0 || volumeDelete < 0 {
		t.Fatalf("missing required operation: %v", client.actions)
	}
	if instanceDelete > detach || detach > volumeDelete || volumeDelete > poolDelete {
		t.Fatalf("destructive operations ran in an unsafe order: %v", client.actions)
	}
	// The default profile itself must survive a wipe.
	if client.has("DELETE /1.0/profiles/default") || client.has("DELETE /1.0/projects/default") {
		t.Fatalf("default scaffolding was deleted: %v", client.actions)
	}
}

// A partial failure must surface instead of reporting a clean node.
func TestResetNodeStateReportsPartialFailure(t *testing.T) {
	client := &fakeResetClient{
		pools:   []string{"/1.0/storage-pools/syspool"},
		profile: map[string]any{"config": map[string]string{}, "devices": map[string]any{}},
		failOn:  "DELETE /1.0/storage-pools/syspool",
	}
	if _, err := resetNodeState(context.Background(), client); err == nil {
		t.Fatal("partial reset must return an error")
	}
}
