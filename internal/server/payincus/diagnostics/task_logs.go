package diagnostics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// InstanceName restricts instance names used for scoped node diagnostics.
var InstanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,99}$`)

// Stream follows bounded instance diagnostics until cancellation.
func Stream(parent context.Context, name string, emit func(string)) {
	ctx, cancel := context.WithTimeout(parent, 15*time.Minute)
	defer cancel()
	var mu sync.Mutex
	count := 0
	send := func(line string) {
		mu.Lock()
		defer mu.Unlock()
		if count >= 4000 {
			return
		}

		count++
		if len(line) > 7500 {
			line = line[:7500]
		}

		emit(time.Now().UTC().Format(time.RFC3339) + " " + line)
	}

	send("节点日志采集已启动: " + name)
	// Incus already records these files on the node. Persist the scoped copy in the panel task log.
	done := make(chan struct{})
	go func() { defer close(done); followQemuLog(ctx, name, send) }()
	defer func() { cancel(); <-done }()
	cmd := exec.CommandContext(ctx, "journalctl", "-u", "incus", "--since", "1 minute ago", "-f", "--no-pager", "-o", "short-iso")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		send("无法读取 Incus journal: " + err.Error())
		<-ctx.Done()
		return
	}
	{
		err := cmd.Start()
		if err != nil {
			send("无法启动 Incus journal 采集: " + err.Error())
			<-ctx.Done()
			return
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	matcher := taskInstanceMatcher(name)
	for scanner.Scan() {
		line := scanner.Text()
		if matcher.MatchString(line) {
			send("[incus] " + line)
		}
	}
	{
		err := scanner.Err()
		if err != nil && ctx.Err() == nil {
			send("Incus journal 读取中断: " + err.Error())
		}
	}
	{
		err := cmd.Wait()
		if err != nil && ctx.Err() == nil {
			send("Incus journal 退出: " + err.Error() + " " + stderr.String())
		}
	}

	if ctx.Err() == nil {
		send("Incus journal 采集结束；继续等待 QEMU 日志")
		<-ctx.Done()
	}
}

func followQemuLog(ctx context.Context, name string, send func(string)) {
	followQemuLogFile(ctx, filepath.Join("/var/log/incus", name, "qemu.log"), send)
}

func followQemuLogFile(ctx context.Context, path string, send func(string)) {
	var offset int64
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	reportedError := false
	for {
		file, err := os.Open(path)
		if err == nil {
			info, statErr := file.Stat()
			if statErr == nil {
				if info.Size() < offset {
					offset = 0
				}
				// Bound reads even if a VM generates huge logs. Do not keep file descriptors across rotation.
				if info.Size()-offset > 64*1024 {
					offset = info.Size() - 64*1024
					send("[qemu] 日志较大，仅采集最近 64 KiB")
				}

				_, _ = file.Seek(offset, io.SeekStart)
				data, readErr := io.ReadAll(io.LimitReader(file, 64*1024))
				offset += int64(len(data))
				if readErr != nil {
					send("QEMU 日志读取失败: " + readErr.Error())
				}

				for _, line := range strings.Split(string(data), "\n") {
					if line != "" {
						send("[qemu] " + line)
					}
				}
			}

			_ = file.Close()
		} else if !os.IsNotExist(err) && !reportedError {
			reportedError = true
			send(fmt.Sprintf("QEMU 日志不可读: %v", err))
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func taskInstanceMatcher(name string) *regexp.Regexp {
	// Avoid mixing vm-1 diagnostics with vm-10.
	pattern := `(^|[^a-zA-Z0-9_.-])` + regexp.QuoteMeta(name) + `([^a-zA-Z0-9_.-]|$)`
	return regexp.MustCompile(pattern)
}

func containsTaskInstance(line, name string) bool { return taskInstanceMatcher(name).MatchString(line) }
