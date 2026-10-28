package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func enrollmentFixture(t *testing.T, c connection) (string, []byte, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	_, fingerprint, err := parseCertificate(certificate)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"schemaVersion": 1, "agentId": c.ID, "agentSecret": c.Secret, "panelClientCertificate": string(certificate), "panelClientCertificateFingerprint": fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	return string(data), certificate, fingerprint
}

func TestEnrollmentCompletesAllStepsAndPreservesTrust(t *testing.T) {
	for _, alreadyTrusted := range []bool{false, true} {
		c := goodConnection()
		payload, certificate, fingerprint := enrollmentFixture(t, c)
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if r.URL.Query().Get("format") != "json" {
				t.Error("missing enrollment format")
			}
			_, _ = w.Write([]byte(payload))
		}))
		var stages []string
		f := &fakeRunner{reply: func(call call) ([]byte, error) {
			args := strings.Join(call.args, " ")
			switch {
			case call.name == "systemctl" && strings.Contains(args, "ExecStart"):
				return []byte("/usr/libexec/incus/payincus-agent"), nil
			case args == "query /1.0/certificates?recursion=1":
				stages = append(stages, "trust-list")
				if alreadyTrusted {
					return []byte(`[{"fingerprint":"` + fingerprint + `","restricted":false}]`), nil
				}
				return []byte(`[{"fingerprint":"another-existing-certificate"}]`), nil
			case strings.HasPrefix(args, "config trust add-certificate "):
				data, err := os.ReadFile(call.args[3])
				if err != nil || string(data) != string(certificate) {
					t.Error("wrong public certificate imported")
				}
				stages = append(stages, "trust-import")
			case strings.HasPrefix(args, "config set "):
				stages = append(stages, "listener")
			case call.name == "systemctl" && args == "enable incudal-agent":
				stages = append(stages, "enable")
			case call.name == "systemctl" && args == "restart incudal-agent":
				stages = append(stages, "restart")
			case strings.Contains(args, "delete"):
				t.Error("existing trust was removed")
			}
			return nil, nil
		}}
		b := newBackend()
		b.runner = f
		b.path = filepath.Join(t.TempDir(), "config.yaml")
		err := b.enroll(context.Background(), server.URL, "ait_"+strings.Repeat("x", 43))
		server.Close()
		if err != nil || requests != 1 {
			t.Fatalf("enrollment failed or token retried: %v, requests=%d", err, requests)
		}
		wanted := []string{"trust-list", "trust-import", "listener", "enable", "restart"}
		if alreadyTrusted {
			wanted = []string{"trust-list", "listener", "enable", "restart"}
		}
		if !reflect.DeepEqual(stages, wanted) {
			t.Fatal(stages)
		}
		if b.loadConnection() != (connection{Panel: server.URL, ID: c.ID, Secret: c.Secret}) {
			t.Fatal("saved credentials differ from the token exchange")
		}
		stat, err := os.Stat(b.path)
		if err != nil || stat.Mode().Perm() != 0o600 {
			t.Fatal("credentials file is not private")
		}
	}
}

func TestEnrollmentPreflightAndStageFailures(t *testing.T) {
	for _, failure := range []string{"preflight", "network", "trust", "listener", "enable", "restart"} {
		payload, _, _ := enrollmentFixture(t, goodConnection())
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests++; _, _ = w.Write([]byte(payload)) }))
		failureCause := errors.New("fixture permission denied")
		f := &fakeRunner{reply: func(call call) ([]byte, error) {
			args := strings.Join(call.args, " ")
			if strings.Contains(args, "ExecStart") {
				return []byte("/usr/libexec/incus/payincus-agent"), nil
			}
			if failure == "preflight" && strings.HasPrefix(args, "config get") {
				return []byte("[::]:8443"), nil
			}
			if failure == "network" && args == "network ensure-panel-bridge" {
				return nil, failureCause
			}
			if strings.HasPrefix(args, "query /1.0/certificates") {
				if failure == "trust" {
					return nil, failureCause
				}
				return []byte("[]"), nil
			}
			if (failure == "listener" && strings.HasPrefix(args, "config set")) || (failure == "enable" && args == "enable incudal-agent") || (failure == "restart" && args == "restart incudal-agent") {
				return nil, failureCause
			}
			return nil, nil
		}}
		b := newBackend()
		b.runner = f
		b.path = filepath.Join(t.TempDir(), "config.yaml")
		err := b.enroll(context.Background(), server.URL, "ait_"+strings.Repeat("x", 43))
		server.Close()
		if err == nil || (failure != "preflight" && !errors.Is(err, failureCause)) {
			t.Fatalf("stage failure swallowed: %s: %v", failure, err)
		}
		if ((failure == "preflight" || failure == "network") && requests != 0) || requests > 1 {
			t.Fatal("preflight consumed token or token request was retried")
		}
		if failure == "trust" || failure == "listener" {
			if _, statErr := os.Stat(b.path); !os.IsNotExist(statErr) {
				t.Fatal("saved credentials before trust/listener succeeded")
			}
		}
	}
}

func TestMenuGroupsKeepMaintenanceAndZFSOffHome(t *testing.T) {
	m := model{backend: &backend{runner: &fakeRunner{}}, height: 50}
	root := m.View()
	if strings.Contains(root, "导入 Panel 客户端证书") || strings.Contains(root, "安装并检查 ZFS") || !strings.Contains(root, "ZFS") || !strings.Contains(root, "更多") {
		t.Fatal("home menu was not consolidated")
	}
	for _, group := range []string{"zfs", "advanced", "repair", "update"} {
		for i, item := range menu {
			if item.action == group {
				m.cursor = i
			}
		}
		updated, cmd := m.selectMenu()
		m = asModel(t, updated)
		if cmd != nil || m.menuGroup != group || m.cursor != 0 {
			t.Fatal("submenu entry executed an operation")
		}
		if group == "zfs" && len(m.currentMenu()) != 8 {
			t.Fatal("ZFS operations missing")
		}
		updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		m = asModel(t, updated)
		if cmd != nil || m.menuGroup != "" || menu[m.cursor].action != group {
			t.Fatal("submenu back navigation lost the home selection")
		}
	}
}

func TestEnrollmentWithLivePanel(t *testing.T) {
	panel := os.Getenv("INCUS_TUI_ENROLL_TEST_PANEL")
	token := os.Getenv("INCUS_TUI_ENROLL_TEST_TOKEN")
	if panel == "" || token == "" {
		t.Skip("set explicit local panel and disposable enrollment token for protocol integration")
	}
	var stages []string
	b := newBackend()
	b.path = filepath.Join(t.TempDir(), "config.yaml")
	b.runner = &fakeRunner{reply: func(c call) ([]byte, error) {
		args := strings.Join(c.args, " ")
		if strings.Contains(args, "ExecStart") {
			return []byte("/usr/libexec/incus/payincus-agent"), nil
		}
		if args == "query /1.0/certificates?recursion=1" {
			return []byte("[]"), nil
		}
		if strings.HasPrefix(args, "config trust add-certificate ") {
			_, _, err := readCertificate(c.args[3])
			if err != nil {
				return nil, err
			}
			stages = append(stages, "trust")
		}
		if strings.HasPrefix(args, "config set ") {
			stages = append(stages, "listener")
		}
		if c.name == "systemctl" && args == "restart incudal-agent" {
			stages = append(stages, "restart")
		}
		return nil, nil
	}}
	var logs strings.Builder
	ctx := context.WithValue(context.Background(), operationLogKey{}, &operationLog{writer: &logs})
	err := b.enroll(ctx, panel, token)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stages, []string{"trust", "listener", "restart"}) {
		t.Fatal("incomplete live protocol enrollment")
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), b.loadConnection().Secret) {
		t.Fatal("live enrollment credentials leaked into logs")
	}
}
