package main

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestInitializationResumesPartialResourcesWithoutDeletingThem(t *testing.T) {
	for _, failedStage := range []string{"network", "storage", "profile"} {
		t.Run(failedStage, func(t *testing.T) {
			// The network case reproduces the old storage-first partial installation.
			poolExists := failedStage == "network"
			bridgeExists := false
			failed := false
			poolCreates, bridgeCreates := 0, 0
			profileDevices := map[string]map[string]string{}
			c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					t.Error("initialization deleted a resource")
				}
				if r.Method == "GET" {
					switch r.URL.Path {
					case "/1.0/storage-pools":
						pools := []string{}
						if poolExists {
							pools = append(pools, "/1.0/storage-pools/default")
						}
						syncJSON(w, pools)
					case "/1.0/storage-pools/default":
						syncJSON(w, map[string]string{"driver": "dir"})
					case "/1.0/networks":
						networks := []nodeNetwork{}
						if bridgeExists {
							networks = append(networks, nodeNetwork{Name: panelBridge, Type: "bridge", Managed: true, Config: panelBridgeConfig()})
						}
						syncJSON(w, networks)
					case "/1.0/profiles/default":
						w.Header().Set("ETag", "default-profile")
						syncJSON(w, map[string]any{"config": map[string]string{}, "devices": profileDevices, "description": "preserve description"})
					default:
						t.Error("unexpected initialization read", r.URL.Path)
					}
					return
				}
				stage := "profile"
				if r.URL.Path == "/1.0/networks" {
					stage = "network"
				}
				if r.URL.Path == "/1.0/storage-pools" {
					stage = "storage"
				}
				if stage == failedStage && !failed {
					failed = true
					w.WriteHeader(500)
					_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": "fixture initialization failure at " + stage})
					return
				}
				switch stage {
				case "network":
					if bridgeExists {
						t.Error("recreated an existing network")
					}
					bridgeExists = true
					bridgeCreates++
				case "storage":
					if poolExists {
						t.Error("recreated an existing pool")
					}
					poolExists = true
					poolCreates++
				case "profile":
					if r.Header.Get("If-Match") != "default-profile" {
						t.Error("profile update lost ETag")
					}
					var body struct {
						Devices     map[string]map[string]string `json:"devices"`
						Description string                       `json:"description"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.Description != "preserve description" {
						t.Error("profile metadata lost")
					}
					profileDevices = body.Devices
				}
				syncJSON(w, nil)
			})
			_, err := c.initializeNode(context.Background())
			if err == nil || !strings.Contains(err.Error(), "fixture initialization failure") {
				t.Fatal("original failure lost", err)
			}
			for range 2 {
				_, err := c.initializeNode(context.Background())
				if err != nil {
					t.Fatal("could not resume initialization", err)
				}
			}
			if bridgeCreates != 1 || poolCreates > 1 || !reflect.DeepEqual(profileDevices, initialDevices()) {
				t.Fatal("partial resources were duplicated or profile was not completed")
			}
		})
	}
}

func TestEnrollmentBridgePreflightDoesNotOverwriteUnexpectedNetwork(t *testing.T) {
	writes := 0
	c := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
		}
		syncJSON(w, []nodeNetwork{{Name: panelBridge, Managed: true, Type: "bridge", Config: map[string]string{"ipv4.address": "192.0.2.1/24"}}})
	})
	err := c.ensurePanelNetwork(context.Background())
	if err == nil || !strings.Contains(err.Error(), "修复") || writes != 0 {
		t.Fatal("enrollment replaced a conflicting bridge", err)
	}
}

func TestBackendInitializationAcceptsKnownPartialState(t *testing.T) {
	f := &fakeRunner{reply: func(c call) ([]byte, error) {
		switch strings.Join(c.args, " ") {
		case "query /1.0/storage-pools":
			return []byte(`["/1.0/storage-pools/default"]`), nil
		case "query /1.0/storage-pools/default":
			return []byte(`{"driver":"dir"}`), nil
		case "query /1.0/networks?recursion=1":
			return json.Marshal([]nodeNetwork{{Name: panelBridge, Managed: true, Type: "bridge", Config: panelBridgeConfig()}})
		case "query /1.0/profiles/default":
			return json.Marshal(map[string]any{"config": map[string]string{}, "devices": initialDevices()})
		}
		return fixture(c)
	}}
	err := (&backend{runner: f}).initialize(context.Background())
	if err != nil {
		t.Fatal("known partial state cannot be resumed", err)
	}
}
