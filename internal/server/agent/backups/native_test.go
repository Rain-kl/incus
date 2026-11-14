package backups

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	incus "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

func TestNativeAPIInventoryAndStreamingExport(t *testing.T) {
	var request api.InstanceBackupsPost
	var exported bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var metadata any
		switch {
		case r.URL.Path == "/1.0":
			metadata = map[string]any{"api_extensions": []string{"instances", "instance_all_projects", "storage", "direct_backup"}, "environment": map[string]any{"server_name": "node-a", "server_clustered": true}}
		case r.URL.Path == "/1.0/instances":
			metadata = []map[string]any{{"name": "local", "project": "default", "location": "node-a"}, {"name": "remote", "project": "other", "location": "node-b"}}
		case r.URL.Path == "/1.0/storage-pools":
			metadata = []any{}
		case r.URL.Path == "/1.0/instances/local/backups" && r.Method == "POST":
			if r.URL.Query().Get("project") != "default" || r.Header.Get("Accept") != "application/octet-stream" {
				t.Error("stream contract changed")
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			exported = true
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = fmt.Fprint(w, "real upstream client stream")
			return
		default:
			t.Errorf("unexpected endpoint %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "metadata": metadata})
	}))
	defer server.Close()
	source := nativeSource{connect: func(ctx context.Context) (incus.InstanceServer, error) {
		return incus.ConnectIncusWithContext(ctx, server.URL, &incus.ConnectionArgs{InsecureSkipVerify: true, SkipGetEvents: true})
	}}
	items, err := source.Inventory(context.Background(), t.TempDir())
	if err != nil || len(items) != 1 || items[0].Name != "local" {
		t.Fatal(items, err)
	}
	file, err := os.CreateTemp(t.TempDir(), "export")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err = source.Export(context.Background(), items[0], file); err != nil {
		t.Fatal(err)
	}
	if request.Name != "" {
		t.Fatalf("direct_backup must not set backup name, got %q", request.Name)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "real upstream client stream" || !exported {
		t.Fatal(err, string(data))
	}
	if !request.InstanceOnly || request.OptimizedStorage || request.RootOnly || request.CompressionAlgorithm != "gzip" {
		t.Fatal(request)
	}
}

func TestNativeImportReportsOperationStage(t *testing.T) {
	opID := "11111111-1111-1111-1111-111111111111"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "sync",
				"metadata": map[string]any{"api_extensions": []string{"instances", "storage", "container_backup", "container_backup_override_pool", "backup_override_name"}, "environment": map[string]any{"server_name": "node-a"}},
			})
		case r.URL.Path == "/1.0/instances" && r.Method == "POST":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":      "async",
				"operation": "/1.0/operations/" + opID,
				"metadata": map[string]any{
					"id":          opID,
					"class":       "task",
					"status":      "Success",
					"status_code": 200,
					"resources":   map[string][]string{"instances": {"/1.0/instances/restored-vm"}},
				},
			})
		case r.URL.Path == "/1.0/storage-pools" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "sync",
				"metadata": []string{"/1.0/storage-pools/default"},
			})
		case r.URL.Path == "/1.0/operations/"+opID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "sync",
				"metadata": map[string]any{
					"id":          opID,
					"class":       "task",
					"status":      "Success",
					"status_code": 200,
					"resources":   map[string][]string{"instances": {"/1.0/instances/restored-vm"}},
				},
			})
		case r.URL.Path == "/1.0/operations/"+opID+"/wait":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type": "sync",
				"metadata": map[string]any{
					"id":          opID,
					"class":       "task",
					"status":      "Success",
					"status_code": 200,
					"resources":   map[string][]string{"instances": {"/1.0/instances/restored-vm"}},
				},
			})
		default:
			t.Errorf("unexpected endpoint %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()

	source := nativeSource{connect: func(ctx context.Context) (incus.InstanceServer, error) {
		return incus.ConnectIncusWithContext(ctx, server.URL, &incus.ConnectionArgs{InsecureSkipVerify: true, SkipGetEvents: true})
	}}
	var mu sync.Mutex
	var stages []string
	ctx := withProgressReporter(context.Background(), func(msg string) {
		mu.Lock()
		stages = append(stages, msg)
		mu.Unlock()
	})
	name, err := source.Import(ctx, strings.NewReader("archive"), "", "default")
	if err != nil {
		t.Fatalf("Import failed: %v", err)
	}
	if name != "restored-vm" {
		t.Fatalf("expected restored-vm, got %q", name)
	}
	mu.Lock()
	joined := strings.Join(stages, "|")
	mu.Unlock()
	if !strings.Contains(joined, "Importing: unpacking instance on host") {
		t.Fatalf("expected unpacking progress stage, got %q", joined)
	}
}

func TestNativeImportResolvesFallbackPoolAndFailsFastOnMissingPool(t *testing.T) {
	opID := "22222222-2222-2222-2222-222222222222"
	var seenPoolHeader string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/1.0":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "sync",
				"metadata": map[string]any{"api_extensions": []string{"instances", "storage", "container_backup", "container_backup_override_pool", "backup_override_name"}, "environment": map[string]any{"server_name": "node-a"}},
			})
		case r.URL.Path == "/1.0/storage-pools" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "sync",
				"metadata": []string{"/1.0/storage-pools/nvme-pool"},
			})
		case r.URL.Path == "/1.0/profiles/default" && r.Method == "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":     "sync",
				"metadata": map[string]any{"name": "default", "devices": map[string]any{}},
			})
		case r.URL.Path == "/1.0/instances" && r.Method == "POST":
			seenPoolHeader = r.Header.Get("X-Incus-pool")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":      "async",
				"operation": "/1.0/operations/" + opID,
				"metadata": map[string]any{
					"id":          opID,
					"class":       "task",
					"status":      "Success",
					"status_code": 200,
					"resources":   map[string][]string{"instances": {"/1.0/instances/u3-ok"}},
				},
			})
		default:
			t.Errorf("unexpected endpoint %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected", 500)
		}
	}))
	defer server.Close()

	source := nativeSource{connect: func(ctx context.Context) (incus.InstanceServer, error) {
		return incus.ConnectIncusWithContext(ctx, server.URL, &incus.ConnectionArgs{InsecureSkipVerify: true, SkipGetEvents: true})
	}}

	if _, err := source.Import(context.Background(), strings.NewReader("archive"), "u3-ok", "missing-pool"); err == nil || !strings.Contains(err.Error(), "missing-pool") {
		t.Fatalf("expected immediate missing pool error, got %v", err)
	}
	if seenPoolHeader != "" {
		t.Fatalf("must not upload archive when target pool is missing, saw header %q", seenPoolHeader)
	}

	name, err := source.Import(context.Background(), strings.NewReader("archive"), "u3-ok", "")
	if err != nil {
		t.Fatalf("fallback import failed: %v", err)
	}
	if name != "u3-ok" || seenPoolHeader != "nvme-pool" {
		t.Fatalf("expected fallback X-Incus-pool nvme-pool, got name=%q header=%q", name, seenPoolHeader)
	}
}
