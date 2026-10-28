package egress

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Release builds populate the ignored archives using scripts/prepare-xray.py.
// Ordinary source builds compile without archives and report Xray unavailable.
//
//go:embed assets/*
var assets embed.FS

type assetManifest struct {
	Version string `json:"version"`
	Files   map[string]struct {
		BinarySHA256 string `json:"binarySha256"`
	} `json:"files"`
}

func manifest() (assetManifest, error) {
	var m assetManifest
	b, err := assets.ReadFile("assets/manifest.json")
	if err == nil {
		err = json.Unmarshal(b, &m)
	}

	return m, err
}

// Available reports whether the pinned Xray payload is embedded.
func Available() bool {
	_, err := assets.ReadFile("assets/xray-linux-" + runtime.GOARCH + ".gz")
	return runtime.GOOS == "linux" && err == nil
}

type binaryVerification struct {
	identity string
	checked  time.Time
}

var verifiedBinaries = struct {
	sync.Mutex
	files map[string]binaryVerification
}{files: map[string]binaryVerification{}}

func binaryIdentity(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("invalid Xray executable")
	}
	// Sys includes inode and ctime on Linux, so replacing a file or restoring mtime invalidates the cache.
	return fmt.Sprintf("%v:%d:%v:%v", info.Sys(), info.Size(), info.ModTime(), info.Mode()), nil
}

func extractBinary(dir string) (string, error) {
	verifiedBinaries.Lock()
	defer verifiedBinaries.Unlock()
	m, err := manifest()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "xray-"+m.Version+"-"+runtime.GOARCH)
	identity, statErr := binaryIdentity(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	previous, exists := verifiedBinaries.files[path]
	if statErr == nil && exists && previous.identity == identity && time.Since(previous.checked) < 5*time.Minute {
		return path, nil
	}
	path, err = extractVerifiedBinary(dir)
	if err != nil {
		delete(verifiedBinaries.files, path)
		return "", err
	}
	identity, err = binaryIdentity(path)
	if err != nil {
		return "", err
	}
	if len(verifiedBinaries.files) >= 128 {
		clear(verifiedBinaries.files)
	}
	verifiedBinaries.files[path] = binaryVerification{identity: identity, checked: time.Now()}
	return path, nil
}

func extractVerifiedBinary(dir string) (string, error) {
	m, err := manifest()
	if err != nil {
		return "", err
	}

	expected := m.Files[runtime.GOARCH].BinarySHA256
	if len(expected) != 64 {
		return "", fmt.Errorf("missing pinned Xray digest")
	}

	path := filepath.Join(dir, "xray-"+m.Version+"-"+runtime.GOARCH)
	{
		b, e := os.ReadFile(path)
		if e == nil {
			sum := sha256.Sum256(b)
			if hex.EncodeToString(sum[:]) == expected {
				{
					err := os.Chmod(path, 0o700)
					if err != nil {
						return "", err
					}
				}
				return path, nil
			}
		}
	}

	compressed, err := assets.ReadFile("assets/xray-linux-" + runtime.GOARCH + ".gz")
	if err != nil {
		return "", err
	}

	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return "", err
	}

	defer func() { _ = r.Close() }()
	b, err := io.ReadAll(io.LimitReader(r, 128<<20))
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != expected {
		return "", fmt.Errorf("Xray digest mismatch")
	}
	{
		err = atomicFile(path, b, 0o700)
		if err != nil {
			return "", err
		}
	}

	for _, name := range []string{"LICENSE", "README.txt"} {
		notice, e := assets.ReadFile("assets/" + name)
		if e != nil {
			return "", e
		}
		{
			e = atomicFile(filepath.Join(dir, "XRAY-"+name), notice, 0o644)
			if e != nil {
				return "", e
			}
		}
	}

	return path, nil
}

func atomicFile(path string, body []byte, mode os.FileMode) error {
	{
		err := os.MkdirAll(filepath.Dir(path), 0o700)
		if err != nil {
			return err
		}
	}

	f, err := os.CreateTemp(filepath.Dir(path), ".network-*")
	if err != nil {
		return err
	}

	defer os.Remove(f.Name())
	{
		err = f.Chmod(mode)
		if err == nil {
			_, err = f.Write(body)
		}
	}

	if err == nil {
		err = f.Sync()
	}

	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}

	if err == nil {
		err = os.Rename(f.Name(), path)
	}

	return err
}
