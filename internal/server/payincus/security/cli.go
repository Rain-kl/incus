//go:build linux

package security

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ObserveCLI runs before upstream aliases or command handlers; output is intentionally generic.
func ObserveCLI(args []string) error {
	category, critical := Command(args)
	socket := filepath.Join(filepath.Dir(Directory()), "payincus-audit.socket")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	defer client.CloseIdleConnections()
	data, _ := json.Marshal(map[string]any{"args": []string{category}})
	request, _ := http.NewRequestWithContext(ctx, "POST", "http://unix/audit", bytes.NewReader(data))
	response, err := client.Do(request)
	if err == nil {
		response.Body.Close()
		if response.StatusCode == 200 {
			return nil
		}
		return PublicError()
	}
	// Root can queue events before daemon startup; no mode bypass occurs while offline.
	blocked, err := Default().Audit(category, critical, os.Getuid(), os.Getpid())
	if err != nil || blocked {
		return PublicError()
	}
	return nil
}
