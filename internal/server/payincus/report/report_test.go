package report

import (
	"runtime"
	"testing"
)

func TestHeartbeatPayloadIncludesHostMetrics(t *testing.T) {
	payload := HeartbeatPayload("test-version", 30)

	capabilities, ok := payload["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities missing or invalid: %#v", payload["capabilities"])
	}

	if !containsCapability(capabilities, "host-metrics") {
		t.Fatalf("host-metrics capability missing: %#v", capabilities)
	}

	if !containsCapability(capabilities, "instance-status") {
		t.Fatalf("instance-status capability missing: %#v", capabilities)
	}

	if !containsCapability(capabilities, "traffic-counters") {
		t.Fatalf("traffic-counters capability missing: %#v", capabilities)
	}

	instances, ok := payload["instances"].(map[string]any)
	if !ok {
		t.Fatalf("instances report missing or invalid: %#v", payload["instances"])
	}

	for _, key := range []string{"available", "reportedAt", "total", "items"} {
		{
			_, ok := instances[key]
			if !ok {
				t.Fatalf("instances key %s missing: %#v", key, instances)
			}
		}
	}

	runtimeInfo, ok := payload["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("runtime missing or invalid: %#v", payload["runtime"])
	}

	for _, key := range []string{"goos", "goarch"} {
		{
			_, ok := runtimeInfo[key]
			if !ok {
				t.Fatalf("runtime key %s missing: %#v", key, runtimeInfo)
			}
		}
	}

	resources, ok := payload["resources"].(map[string]any)
	if !ok {
		t.Fatalf("resources missing or invalid: %#v", payload["resources"])
	}

	for _, key := range []string{
		"cpuTotal",
		"swapTotalMb",
		"swapUsedMb",
		"swapUsagePercent",
		"diskTotalBytes",
		"diskUsedBytes",
		"diskUsagePercent",
	} {
		{
			_, ok := resources[key]
			if !ok {
				t.Fatalf("resource key %s missing: %#v", key, resources)
			}
		}
	}

	if runtime.GOOS == "linux" {
		for _, key := range []string{
			"cpuUsagePercent",
			"memoryTotalMb",
			"memoryUsedMb",
			"memoryUsagePercent",
			"processCount",
		} {
			{
				_, ok := resources[key]
				if !ok {
					t.Fatalf("linux resource key %s missing: %#v", key, resources)
				}
			}
		}
	}

	metrics, ok := payload["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("metrics missing or invalid: %#v", payload["metrics"])
	}

	for _, key := range []string{"reportedAt", "heartbeatIntervalSeconds"} {
		{
			_, ok := metrics[key]
			if !ok {
				t.Fatalf("metric key %s missing: %#v", key, metrics)
			}
		}
	}

	if runtime.GOOS == "linux" {
		for _, key := range []string{"uptimeSeconds", "load1", "load5", "load15"} {
			{
				_, ok := metrics[key]
				if !ok {
					t.Fatalf("linux metric key %s missing: %#v", key, metrics)
				}
			}
		}
	}

	if metrics["heartbeatIntervalSeconds"] != 30 {
		t.Fatalf("heartbeat interval mismatch: %#v", metrics["heartbeatIntervalSeconds"])
	}
}

func TestRunningIncusStatusDetection(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		statusCode int
		expected   bool
	}{
		{name: "running string", status: "Running", statusCode: 0, expected: true},
		{name: "running status code", status: "", statusCode: 103, expected: true},
		{name: "stopped", status: "Stopped", statusCode: 102, expected: false},
		{name: "empty", status: "", statusCode: 0, expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			{
				got := isRunningIncusStatus(tt.status, tt.statusCode)
				if got != tt.expected {
					t.Fatalf("running status mismatch: got=%t want=%t", got, tt.expected)
				}
			}
		})
	}
}

func TestTrafficCountersFromIncusStateUsesBillableInterfaces(t *testing.T) {
	state := incusInstanceState{
		Network: map[string]incusNetworkDevice{
			"lo": {
				Counters: incusNetworkCounters{
					BytesReceived: "999",
					BytesSent:     "999",
				},
			},
			"eth0": {
				Counters: incusNetworkCounters{
					BytesReceived: "100",
					BytesSent:     "200",
				},
			},
			"docker0": {
				Counters: incusNetworkCounters{
					BytesReceived: "300",
					BytesSent:     "400",
				},
			},
		},
	}

	counters := getTrafficCountersFromIncusState("vm-test", state)
	if counters.rx != 100 || counters.tx != 200 {
		t.Fatalf("traffic counters mismatch: got=%+v", counters)
	}
}

func TestTrafficCountersFromIncusStateFallsBackToExternalInterfaces(t *testing.T) {
	state := incusInstanceState{
		Network: map[string]incusNetworkDevice{
			"lo": {
				Counters: incusNetworkCounters{
					BytesReceived: "999",
					BytesSent:     "999",
				},
			},
			"enp5s0": {
				Counters: incusNetworkCounters{
					BytesReceived: "123",
					BytesSent:     "456",
				},
			},
		},
	}

	counters := getTrafficCountersFromIncusState("vm-test", state)
	if counters.rx != 123 || counters.tx != 456 {
		t.Fatalf("traffic counters mismatch: got=%+v", counters)
	}
}

func containsCapability(capabilities []any, expected string) bool {
	for _, capability := range capabilities {
		if capability == expected {
			return true
		}
	}

	return false
}
