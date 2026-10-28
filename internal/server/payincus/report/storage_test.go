package report

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestStorageHeartbeatNativeContracts(t *testing.T) {
	before := inspectZFSStorage
	inspectZFSStorage = func(context.Context, string) (map[string]any, bool) {
		return map[string]any{"sizeBytes": int64(2 << 30), "id": "backing", "availableBytes": int64(1 << 30)}, true
	}
	t.Cleanup(func() { inspectZFSStorage = before })
	locked, unavailable, brokenList := false, false, false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var metadata any
		switch r.URL.Path {
		case "/1.0/storage-pools":
			if brokenList {
				http.Error(w, "unavailable", 503)
				return
			}
			if r.URL.Query().Get("recursion") != "1" {
				t.Error("inventory must use the native recursive API")
			}
			metadata = []reportedPool{{Name: "data", Driver: "zfs", Status: "Created", Config: map[string]string{"zfs.pool_name": "tank/incus"}}, {Name: "plain", Driver: "dir", Status: "Created"}}
		case "/1.0/payincus/storage":
			status := "available"
			if locked {
				status = "unavailable"
			}
			metadata = map[string]any{"datasets": []encryptionDataset{{Dataset: "tank/incus", EncryptionRoot: "tank/incus", Algorithm: "aes-256-gcm", KeyStatus: status, KeyFormat: "hex"}}}
		case "/1.0/storage-pools/data/resources", "/1.0/storage-pools/plain/resources":
			if unavailable {
				http.Error(w, "failed", 503)
				return
			}
			metadata = map[string]any{"space": map[string]any{"total": 2 << 30, "used": 10 << 20}}
		default:
			t.Error("unexpected telemetry API", r.URL.String())
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": metadata})
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteStorageTransport{base: server.URL, next: http.DefaultTransport}
	read := func() map[string]any {
		return storageReport(context.Background(), client, map[string]any{"available": false})
	}
	result := read()
	items := result["pools"].([]map[string]any)
	if result["available"] != true || result["complete"] != true || items[0]["status"] != "ready" || items[1]["status"] != "ready" || items[0]["backing"] == nil {
		t.Fatal("ready pool report invalid", result)
	}
	locked = true
	if read()["pools"].([]map[string]any)[0]["status"] != "locked" {
		t.Fatal("unloaded keys were allocatable")
	}
	locked, unavailable = false, true
	if read()["pools"].([]map[string]any)[0]["status"] != "unavailable" {
		t.Fatal("failed capacity was reported as ready")
	}
	brokenList = true
	if read()["available"] != false {
		t.Fatal("failed inventory was reported as an empty healthy node")
	}
}

type rewriteStorageTransport struct {
	base string
	next http.RoundTripper
}

func (t rewriteStorageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	request, err := http.NewRequestWithContext(r.Context(), r.Method, t.base+r.URL.RequestURI(), nil)
	if err != nil {
		return nil, err
	}
	return t.next.RoundTrip(request)
}

func TestStorageHeartbeatDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	client := server.Client()
	client.Transport = rewriteStorageTransport{base: server.URL, next: http.DefaultTransport}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if storageReport(ctx, client, map[string]any{"available": false})["available"] != false {
		t.Fatal("cancelled inventory was accepted")
	}
}

func TestHardwareDisksTravelInHeartbeatResources(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "incus.socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	before := incusSocketCandidates
	incusSocketCandidates = []string{socket}
	t.Cleanup(func() { incusSocketCandidates = before })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1.0/resources" {
			t.Error("unexpected resource endpoint", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": map[string]any{"storage": map[string]any{"disks": []any{map[string]any{"id": "nvme0n1", "size": int64(100 << 30)}}}, "network": map[string]any{"cards": []any{}}}})
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	resources := collectResources()
	storage, ok := resources["storage"].(map[string]any)
	if !ok || resources["storageAvailable"] != true {
		t.Fatal("native disk inventory not reported", resources)
	}
	disks, ok := storage["disks"].([]any)
	if !ok || len(disks) != 1 || disks[0].(map[string]any)["id"] != "nvme0n1" {
		t.Fatal("disk inventory changed", storage)
	}
}
