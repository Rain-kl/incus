package report

import "testing"

func TestParseNvidiaGPUs(t *testing.T) {
	output := "GPU-11111111-1111-1111-1111-111111111111, 00000000:65:00.0, NVIDIA A10, 23028, 1024, 25, 30, 60, 80.5, 150, 550.54.15, Default\nGPU-22222222-2222-2222-2222-222222222222, 00000000:b3:00.0, NVIDIA A10, 23028, 2048, 80, N/A, 65, [Not Supported], 150, 550.54.15, Default\n"
	cards, err := parseNvidiaGPUs(output)
	if err != nil || len(cards) != 2 {
		t.Fatalf("two GPUs expected: %v %v", cards, err)
	}

	if cards[0]["pciAddress"] != "0000:65:00.0" || cards[1]["pciAddress"] != "0000:b3:00.0" {
		t.Fatal("GPU identity must remain distinct")
	}

	power, ok := cards[1]["powerDrawWatts"].(*float64)
	if !ok {
		t.Fatal("expected nullable power reading")
	}

	if power != nil {
		t.Fatal("unsupported metric must remain null")
	}

	if *cards[0]["gpuUtilization"].(*float64) != 25 {
		t.Fatal("GPU utilization lost")
	}
}

func TestInvalidNvidiaGPUs(t *testing.T) {
	for _, output := range []string{"driver failed", "bad,0000:65:00.0,A10,23028,0,0,0,0,0,0,550,Default", "GPU-11111111-1111-1111-1111-111111111111,0000:65:00.0,A10,N/A,0,0,0,0,0,0,550,Default"} {
		{
			_, err := parseNvidiaGPUs(output)
			if err == nil {
				t.Fatalf("invalid report accepted: %s", output)
			}
		}
	}

	for _, input := range []string{"NaN", "+Inf", "-1", "N/A"} {
		if gpuNumber(input) != nil {
			t.Fatalf("invalid measurement accepted: %s", input)
		}
	}
}
