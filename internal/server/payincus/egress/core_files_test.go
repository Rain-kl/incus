package egress

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreStagingSkipsStableConfiguration(t *testing.T) {
	dir := t.TempDir()
	config, unit := filepath.Join(dir, "config.json"), filepath.Join(dir, "core.service")
	original := []byte(`{"outbounds":[]}`)
	_ = os.WriteFile(config, original, 0o600)
	_ = os.WriteFile(unit, []byte("original-unit"), 0o644)
	before, _ := os.Stat(config)
	// A nonexistent validator proves the unchanged path launches no validation process.
	changed, err := stageCoreFiles(context.Background(), "/nonexistent-validator", config, unit, original, "original-unit", false)
	if err != nil || changed {
		t.Fatalf("unchanged configuration staged: %v", err)
	}
	after, _ := os.Stat(config)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("stable file was rewritten")
	}
	{
		_, err := os.Stat(config + ".next")
		if !os.IsNotExist(err) {
			t.Fatal("stable path created a staging file")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, err := stageCoreFiles(ctx, "/missing", config, unit, original, "original-unit", false)
		if err != context.Canceled {
			t.Fatal("canceled staging did not stop")
		}
	}

	validator := filepath.Join(dir, "validator")
	_ = os.WriteFile(validator, []byte("#!/bin/sh\necho 'fixture-secret' >&2\nexit 1\n"), 0o700)
	changed, err = stageCoreFiles(context.Background(), validator, config, unit, []byte(`{"changed":true}`), "new-unit", false)
	if err == nil || changed || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatal("validation failure was not sanitized")
	}
	actual, _ := os.ReadFile(config)
	if string(actual) != string(original) {
		t.Fatal("invalid configuration replaced last good file")
	}
	actual, _ = os.ReadFile(unit)
	if string(actual) != "original-unit" {
		t.Fatal("invalid configuration changed the service")
	}
	_ = os.WriteFile(validator, []byte("#!/bin/sh\nexit 0\n"), 0o700)
	changed, err = stageCoreFiles(context.Background(), validator, config, unit, original, "original-unit", true)
	if err != nil || !changed {
		t.Fatal("a restarted or unhealthy core must be validated")
	}
}

func TestBinaryVerificationCacheDetectsChanges(t *testing.T) {
	if !Available() {
		t.Skip("pinned core required")
	}
	dir := t.TempDir()
	path, err := extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}
	verifiedBinaries.Lock()
	first := verifiedBinaries.files[path]
	verifiedBinaries.Unlock()
	_, err = extractBinary(dir)
	if err != nil {
		t.Fatal(err)
	}
	verifiedBinaries.Lock()
	second := verifiedBinaries.files[path]
	verifiedBinaries.Unlock()
	if !first.checked.Equal(second.checked) {
		t.Fatal("stable executable was rehashed")
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte("corrupt"), 0)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, err = extractBinary(dir)
	if err != nil {
		t.Fatal("failed to restore pinned core")
	}
	verifiedBinaries.Lock()
	repaired := verifiedBinaries.files[path]
	verifiedBinaries.Unlock()
	if first.identity == repaired.identity {
		t.Fatal("changed binary reused cached verification")
	}
}

func BenchmarkVerifiedBinary(b *testing.B) {
	if !Available() {
		b.Skip("pinned core required")
	}
	dir := b.TempDir()
	{
		_, err := extractBinary(dir)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Run("full-hash", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			{
				_, err := extractVerifiedBinary(dir)
				if err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	{
		_, err := extractBinary(dir)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Run("unchanged", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			{
				_, err := extractBinary(dir)
				if err != nil {
					b.Fatal(err)
				}
			}
		}
	})
}
