//go:build linux

package security

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ObserveCLI runs before upstream handlers; only a confirmed lock uses the generic public error.
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
		defer response.Body.Close()
		if response.StatusCode == 200 {
			return nil
		}
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
		if readErr != nil {
			return fmt.Errorf("cannot read CLI audit response: %w", readErr)
		}
		detail := strings.TrimSpace(string(message))
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("CLI audit HTTP %d: %s", response.StatusCode, detail)
	}
	// Root can queue events before daemon startup; no mode bypass occurs while offline.
	blocked, err := Default().Audit(category, critical, os.Getuid(), os.Getpid())
	if err != nil {
		if errors.Is(Default().Check(), ErrLocked) {
			return PublicError()
		}
		return fmt.Errorf("local CLI audit failed: %w", err)
	}
	if blocked {
		return PublicError()
	}
	return nil
}
