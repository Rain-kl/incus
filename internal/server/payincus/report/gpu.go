package report

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	gpuPCI  = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)
	gpuUUID = regexp.MustCompile(`^GPU-[a-fA-F0-9-]{16,64}$`)
)

// Unsupported measurements stay null; never turn a driver error into zero usage.
func gpuNumber(s string) *float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || value < 0 || value != value || value > 1e12 {
		return nil
	}

	return &value
}

func parseNvidiaGPUs(output string) ([]map[string]any, error) {
	reader := csv.NewReader(strings.NewReader(output))
	reader.TrimLeadingSpace = true
	cards := []map[string]any{}
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}

		if err != nil || len(row) != 12 {
			return nil, fmt.Errorf("invalid NVIDIA GPU report")
		}

		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}

		pci := strings.ToLower(row[1])
		if len(pci) == 16 && strings.HasPrefix(pci, "0000") {
			pci = pci[4:]
		}

		total := gpuNumber(row[3])
		if !gpuUUID.MatchString(row[0]) || !gpuPCI.MatchString(pci) || total == nil || *total <= 0 {
			return nil, fmt.Errorf("invalid NVIDIA GPU identity or memory")
		}

		cards = append(cards, map[string]any{
			"uuid": row[0], "pciAddress": pci, "name": row[2], "memoryTotalMiB": *total,
			"memoryUsedMiB": gpuNumber(row[4]), "gpuUtilization": gpuNumber(row[5]),
			"memoryUtilization": gpuNumber(row[6]), "temperature": gpuNumber(row[7]),
			"powerDrawWatts": gpuNumber(row[8]), "powerLimitWatts": gpuNumber(row[9]),
			"driverVersion": row[10], "computeMode": row[11],
		})
	}

	return cards, nil
}

func collectNvidiaGPUs() map[string]any {
	report := map[string]any{"status": "unavailable", "runtimeReady": false, "cards": []any{}}
	binary, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return report
	}

	_, cliErr := exec.LookPath("nvidia-container-cli")
	report["runtimeReady"] = cliErr == nil
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary,
		"--query-gpu=uuid,pci.bus_id,name,memory.total,memory.used,utilization.gpu,utilization.memory,temperature.gpu,power.draw,power.limit,driver_version,compute_mode",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		report["status"] = "error"
		return report
	}

	cards, err := parseNvidiaGPUs(string(output))
	if err != nil {
		report["status"] = "error"
		return report
	}

	report["status"] = "ready"
	report["cards"] = cards
	return report
}
