package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// ProbeURL uses a local per-outbound SOCKS listener and verifies a real HTTPS
// response. It never treats an open proxy TCP port as successful connectivity.
func ProbeURL(ctx context.Context, port int, target string) ProbeResult {
	result := ProbeResult{Error: "network"}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return result
	}

	transport := &http.Transport{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) { return socksDial(ctx, port, address) }, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return result
	}

	start := time.Now()
	resp, err := client.Do(req)
	result.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		var networkError net.Error
		if ctx.Err() != nil || (errors.As(err, &networkError) && networkError.Timeout()) {
			result.Error = "timeout"
		}

		return result
	}

	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		result.Error = "http"
		return result
	}

	result.Success = true
	result.Error = ""
	return result
}

func socksDial(ctx context.Context, port int, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}

	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	failed := true
	defer func() {
		if failed {
			_ = conn.Close()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	{
		d, ok := ctx.Deadline()
		if ok && d.Before(deadline) {
			deadline = d
		}
	}

	_ = conn.SetDeadline(deadline)
	{
		_, err = conn.Write([]byte{5, 1, 0})
		if err != nil {
			return nil, err
		}
	}

	header := make([]byte, 2)
	{
		_, err = io.ReadFull(conn, header)
		if err != nil || header[0] != 5 || header[1] != 0 {
			return nil, fmt.Errorf("SOCKS handshake")
		}
	}

	host, p, err := net.SplitHostPort(address)
	if err != nil || len(host) > 255 {
		return nil, fmt.Errorf("SOCKS address")
	}

	number, err := strconv.Atoi(p)
	if err != nil {
		return nil, err
	}

	request := append([]byte{5, 1, 0, 3, byte(len(host))}, []byte(host)...)
	request = append(request, byte(number>>8), byte(number))
	{
		_, err = conn.Write(request)
		if err != nil {
			return nil, err
		}
	}

	reply := make([]byte, 4)
	{
		_, err = io.ReadFull(conn, reply)
		if err != nil || reply[0] != 5 || reply[1] != 0 {
			return nil, fmt.Errorf("SOCKS connect")
		}
	}

	length := 0
	switch reply[3] {
	case 1:
		length = 4
	case 4:
		length = 16
	case 3:
		one := make([]byte, 1)
		{
			_, err = io.ReadFull(conn, one)
			if err != nil {
				return nil, err
			}
		}

		length = int(one[0])
	default:
		return nil, fmt.Errorf("SOCKS reply")
	}
	{
		_, err = io.CopyN(io.Discard, conn, int64(length+2))
		if err != nil {
			return nil, err
		}
	}

	_ = conn.SetDeadline(time.Time{})
	failed = false
	return conn, nil
}

// ProbeCLI is executed inside the gateway namespace, without credentials in argv.
func ProbeCLI(port int, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_ = json.NewEncoder(os.Stdout).Encode(ProbeURL(ctx, port, target))
}

func executeProbe(ctx context.Context, dir string, job ProbeRequest) []ProbeResult {
	result := []ProbeResult{}
	for _, n := range job.Nodes {
		result = append(result, ProbeResult{NodeID: n.ID, Error: "xray"})
	}

	binaryPath, err := extractBinary(dir)
	if err != nil {
		return result
	}

	tmp, err := os.MkdirTemp(dir, "probe-")
	if err != nil {
		return result
	}

	defer func() { _ = os.RemoveAll(tmp) }()
	ports := []int{}
	listeners := []net.Listener{}
	for range job.Nodes {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			for _, l := range listeners {
				_ = l.Close()
			}

			return result
		}

		listeners = append(listeners, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}

	for _, l := range listeners {
		_ = l.Close()
	}

	config, err := xrayConfig(Instruction{Nodes: job.Nodes}, ports, false)
	if err != nil {
		return result
	}

	path := filepath.Join(tmp, "config.json")
	if atomicFile(path, config, 0o600) != nil {
		return result
	}
	{
		err = validateCore(ctx, binaryPath, path)
		if err != nil {
			detail := "config_invalid"
			var core *coreValidationError
			if errors.As(err, &core) {
				detail = core.reason
			}

			for i := range result {
				result[i].ErrorDetail = detail
			}

			log.Printf("[egress] realtime probe rejected stage=config reason=%s", detail)
			return result
		}
	}

	cmd := exec.CommandContext(ctx, binaryPath, "run", "-config", path)
	if cmd.Start() != nil {
		return result
	}

	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	var probes sync.WaitGroup
	limit := make(chan struct{}, 4)
	for i, node := range job.Nodes {
		probes.Add(1)
		go func(i int, node Node) {
			defer probes.Done()
			select {
			case limit <- struct{}{}:
			case <-ctx.Done():
				return
			}

			defer func() { <-limit }()
			ready := false
			for attempt := 0; attempt < 30; attempt++ {
				c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ports[i])), 100*time.Millisecond)
				if e == nil {
					_ = c.Close()
					ready = true
					break
				}

				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}

			if !ready {
				return
			}

			pctx, cancel := context.WithTimeout(ctx, 12*time.Second)
			probe := ProbeURL(pctx, ports[i], job.ProbeURL)
			cancel()
			probe.NodeID = node.ID
			result[i] = probe
		}(i, node)
	}

	probes.Wait()
	return result
}
