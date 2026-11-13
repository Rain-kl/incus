package backups

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "real upstream client stream" || !exported {
		t.Fatal(err, string(data))
	}
	if !request.InstanceOnly || request.OptimizedStorage || request.RootOnly || request.CompressionAlgorithm != "gzip" {
		t.Fatal(request)
	}
}
