package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestEncryptedPoolWithoutControlPanel(t *testing.T) {
	for _, mode := range []string{"success", "unsupported", "locked", "create-failed", "bad-identity"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("INCUS_DIR", t.TempDir())
			var logs strings.Builder
			log := &operationLog{writer: &logs}
			ctx := context.WithValue(context.Background(), operationLogKey{}, log)
			key := ""
			client := apiFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/1.0/payincus":
					extensions := []string{"payincus_storage_encryption_v1"}
					if mode == "unsupported" {
						extensions = nil
					}
					syncJSON(w, map[string]any{"api_extensions": extensions})
				case "/1.0/payincus/security":
					syncJSON(w, map[string]bool{"locked": mode == "locked"})
				case "/1.0/payincus/storage/pool":
					var input struct {
						Name    string `json:"name"`
						Device  string `json:"device"`
						SizeGiB uint64 `json:"sizeGiB"`
						Key     string `json:"key"`
					}
					if json.NewDecoder(r.Body).Decode(&input) != nil || input.Name != "test" || len(input.Key) != 64 || input.SizeGiB != 1 || input.Device != "" {
						t.Fatal("invalid standalone creation request")
					}
					key = input.Key
					if mode == "create-failed" {
						w.WriteHeader(409)
						_, _ = w.Write([]byte(`{"error":"dataset creation failed"}`))
						return
					}
					fp := keyFingerprint(key)
					if mode == "bad-identity" {
						fp = "wrong"
					}
					syncJSON(w, map[string]any{"targets": []map[string]string{{"dataset": "test/incus", "fingerprint": fp}}})
				default:
					t.Fatal("unexpected endpoint or control-plane dependency", r.URL.Path)
				}
			})
			f := &fakeRunner{}
			b := &backend{runner: f, logs: log}
			receipt, err := b.createEncryptedPool(ctx, client, encryptedPoolRequest{name: "test", size: "1"})
			if mode == "success" {
				if err != nil || receipt == nil || !receipt.complete || receipt.key != key {
					t.Fatal("encrypted creation failed")
				}
			} else if err == nil {
				t.Fatal("failure reported success")
			}
			if mode == "locked" || mode == "unsupported" {
				if len(f.calls) != 0 || receipt != nil {
					t.Fatal("preflight failure allocated storage or exposed a key")
				}
			} else {
				if receipt == nil || receipt.key != key || !strings.Contains(receipt.view(), key) {
					t.Fatal("key lost after possible dataset creation")
				}
				if strings.Contains(logs.String(), key) {
					t.Fatal("key leaked to session log")
				}
			}
			if len(f.calls) != 0 {
				t.Fatal("TUI bypassed the node storage allocator")
			}
			if mode == "success" {
				m := model{backend: b, busy: true}
				updated, _ := m.Update(resultMsg{pool: receipt})
				m = asModel(t, updated)
				if m.showLogs || !strings.Contains(m.View(), key) || !strings.Contains(m.View(), "test/incus") {
					t.Fatal("result page hid key or correct import source")
				}
				updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				m = asModel(t, updated)
				if m.poolReceipt != nil || strings.Contains(m.View(), key) {
					t.Fatal("dismissed key remained visible")
				}
			}
		})
	}
}
