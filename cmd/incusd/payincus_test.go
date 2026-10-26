package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	incus "github.com/lxc/incus/v7/client"
)

func TestPayIncusAuthenticatedRoute(t *testing.T) {
	t.Setenv("INCUS_PAYINCUS", "1")
	daemon, cleanup := newTestDaemon(t)
	defer cleanup()
	client, err := incus.ConnectIncusUnix(daemon.os.GetUnixSocket(), nil)
	if err != nil {
		t.Fatal(err)
	}

	defer client.Disconnect()
	result, _, err := client.RawQuery("GET", "/1.0/payincus", nil, "")
	if err != nil {
		t.Fatal(err)
	}

	var metadata struct {
		Extensions []string `json:"api_extensions"`
	}

	err = json.Unmarshal(result.Metadata, &metadata)
	if err != nil || !slices.Contains(metadata.Extensions, "payincus_node_v1") || !slices.Contains(metadata.Extensions, "payincus_security_v1") {
		t.Fatalf("unexpected capabilities %s: %v", result.Metadata, err)
	}
	// Exercise a child route through the actual router, not just the standalone service.
	_, _, err = client.RawQuery("GET", "/1.0/payincus/security", nil, "")
	if err != nil {
		t.Fatalf("child route unavailable: %v", err)
	}

	// The same route must reject unauthenticated HTTP requests before invoking extensions.
	server := httptest.NewServer(restServer(daemon).Handler)
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/1.0/payincus")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status %d", resp.StatusCode)
	}
}
