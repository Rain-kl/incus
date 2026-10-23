package diagnostics

import (
	"bufio"
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// Journal accepts only fixed service units, never arbitrary journalctl arguments.
func Journal(ctx context.Context, unit string, lines int, emit func(string)) {
	if (unit != "incus" && unit != "incudal-agent") || lines < 1 || lines > 1000 {
		return
	}

	cmd := exec.CommandContext(ctx, "journalctl", "-u", unit, "-n", strconv.Itoa(lines), "-f", "--no-pager", "-o", "short-iso")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}

	err = cmd.Start()
	if err != nil {
		return
	}

	defer func() { _ = cmd.Wait() }()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 8192), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) > 8000 {
			line = line[:8000]
		}

		if line != "" {
			emit(line)
		}
	}
}
