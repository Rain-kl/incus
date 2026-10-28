//go:build linux

package security

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Runner executes storage commands with key material provided only over stdin.
type (
	Runner func(context.Context, string, []string, []byte) ([]byte, error)
	// Storage adapts ZFS operations without owning plaintext key files.
	Storage struct {
		Run       Runner
		ImportDir string
	}
)

var encryptionRootPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)*$`)

var datasetPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:-]*(/[A-Za-z0-9_.:-]+)+$`)

// ValidDataset accepts dedicated child dataset names only.
func ValidDataset(name string) bool {
	return len(name) <= 200 && datasetPattern.MatchString(name) && !strings.Contains(name, "..")
}

// ValidEncryptionRoot accepts existing dedicated roots, including an externally created pool root.
func ValidEncryptionRoot(name string) bool {
	return len(name) <= 200 && encryptionRootPattern.MatchString(name) && !strings.Contains(name, "..")
}

// KeyFingerprint identifies a random hex key without retaining its bytes.
func KeyFingerprint(key string) (string, error) {
	data, err := hex.DecodeString(key)
	if err != nil || len(data) != 32 {
		return "", errors.New("expected a 64-character random ZFS hex key")
	}
	digest := sha256.Sum256(data)
	clear(data)
	return hex.EncodeToString(digest[:]), nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	take := n
	if take > w.remaining {
		take = w.remaining
	}
	if take > 0 {
		_, err := w.writer.Write(p[:take])
		if err != nil {
			return 0, err
		}
		w.remaining -= take
	}
	return n, nil
}

// Run executes storage helpers with bounded output and no stderr capture.
func Run(ctx context.Context, name string, args []string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	cmd.Stdout = &limitedWriter{writer: &output, remaining: 1 << 20}
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return output.Bytes(), err
}

func (s *Storage) property(ctx context.Context, dataset, property string) (string, error) {
	if !ValidEncryptionRoot(dataset) {
		return "", errors.New("invalid protected dataset; use a dedicated child dataset")
	}
	output, err := s.Run(ctx, "zfs", []string{"get", "-H", "-o", "value", property, dataset}, nil)
	if err != nil {
		return "", errors.New("ZFS property unavailable: " + property)
	}
	return strings.TrimSpace(string(output)), nil
}

// Ready verifies encryption identity and available key state.
func (s *Storage) Ready(ctx context.Context, target Target) error {
	if len(target.Fingerprint) != 64 {
		return errors.New("invalid key fingerprint")
	}
	if target.GUID != "" {
		guid, err := s.property(ctx, target.Dataset, "guid")
		if err != nil || guid != target.GUID {
			return errors.New("ZFS identity changed")
		}
	}
	for property, expected := range map[string]string{"encryptionroot": target.Dataset, "keyformat": "hex", "keystatus": "available"} {
		actual, err := s.property(ctx, target.Dataset, property)
		if err != nil {
			return err
		}
		if actual != expected {
			return errors.New("ZFS encryption identity or key state mismatch: " + property)
		}
	}
	encryption, err := s.property(ctx, target.Dataset, "encryption")
	if err != nil {
		return err
	}
	if encryption != "aes-256-gcm" {
		return errors.New("expected aes-256-gcm dataset")
	}
	if err := s.safeMountpoint(ctx, target.Dataset); err != nil {
		return err
	}
	location, err := s.property(ctx, target.Dataset, "keylocation")
	if err != nil {
		return err
	}
	if location != "prompt" {
		return errors.New("keylocation must be prompt; persistent key sources are not accepted")
	}
	return nil
}

func (s *Storage) safeMountpoint(ctx context.Context, dataset string) error {
	mountpoint, err := s.property(ctx, dataset, "mountpoint")
	if err != nil {
		return err
	}
	switch mountpoint {
	case "/", "/etc", "/usr", "/var", "/home", "/boot", "/bin", "/sbin", "/lib", "/lib64", "/opt":
		return errors.New("host system filesystem cannot be managed as an instance encryption root")
	}
	return nil
}

// Unlock loads or verifies a supplied key without clearing the node lock.
// UnlockManaged also adopts a verified external key into prompt-only loading.
func (s *Storage) UnlockManaged(ctx context.Context, target Target, key []byte) error {
	return s.unlock(ctx, target, key, true)
}

func (s *Storage) Unlock(ctx context.Context, target Target, key []byte) error {
	return s.unlock(ctx, target, key, false)
}

func (s *Storage) unlock(ctx context.Context, target Target, key []byte, adopt bool) error {
	if !ValidEncryptionRoot(target.Dataset) {
		return errors.New("invalid protected dataset")
	}
	fingerprint, err := KeyFingerprint(strings.TrimSpace(string(key)))
	if err != nil {
		return err
	}
	if fingerprint != target.Fingerprint {
		return errors.New("key fingerprint does not match registered dataset")
	}
	status, err := s.property(ctx, target.Dataset, "keystatus")
	if err != nil {
		pool := strings.Split(target.Dataset, "/")[0]
		_, err = s.Run(ctx, "zpool", []string{"import", "-N", pool}, nil)
		if err != nil && s.ImportDir != "" {
			// File-backed pools created by the TUI are outside ZFS's default device search path.
			_, err = s.Run(ctx, "zpool", []string{"import", "-N", "-d", filepath.Clean(s.ImportDir), pool}, nil)
		}
		if err != nil {
			return errors.New("pool import failed")
		}
		status, err = s.property(ctx, target.Dataset, "keystatus")
		if err != nil {
			return err
		}
	}
	if target.GUID != "" {
		guid, err := s.property(ctx, target.Dataset, "guid")
		if err != nil || guid != target.GUID {
			return errors.New("ZFS identity changed")
		}
	}
	if adopt {
		if err := s.safeMountpoint(ctx, target.Dataset); err != nil {
			return err
		}
		for property, expected := range map[string]string{"encryptionroot": target.Dataset, "keyformat": "hex", "encryption": "aes-256-gcm"} {
			actual, err := s.property(ctx, target.Dataset, property)
			if err != nil || actual != expected {
				return errors.New("unsupported external encryption root")
			}
		}
	}
	args := []string{"load-key", "-L", "prompt", target.Dataset}
	if status == "available" {
		args = []string{"load-key", "-n", "-L", "prompt", target.Dataset}
	}
	_, err = s.Run(ctx, "zfs", args, append(bytes.TrimSpace(key), '\n'))
	if err != nil {
		return errors.New("ZFS key validation/loading failed")
	}
	if adopt {
		location, err := s.property(ctx, target.Dataset, "keylocation")
		if err != nil {
			return err
		}
		if location != "prompt" {
			if _, err = s.Run(ctx, "zfs", []string{"set", "keylocation=prompt", target.Dataset}, nil); err != nil {
				return errors.New("cannot configure prompt-only key loading")
			}
		}
	}
	return s.Ready(ctx, target)
}

// Create creates a new encrypted dataset without replacing existing storage.
func (s *Storage) Create(ctx context.Context, dataset string, key []byte) error {
	_, err := KeyFingerprint(strings.TrimSpace(string(key)))
	if err != nil {
		return err
	}
	if !ValidDataset(dataset) {
		return errors.New("invalid dataset")
	}
	_, err = s.Run(ctx, "zfs", []string{"create", "-o", "encryption=aes-256-gcm", "-o", "keyformat=hex", "-o", "keylocation=prompt", dataset}, append(bytes.TrimSpace(key), '\n'))
	if err != nil {
		return errors.New("ZFS dataset creation failed; no existing data is overwritten")
	}
	return nil
}

// Lock unmounts descendants and verifies key removal.
func (s *Storage) Lock(ctx context.Context, target Target) error {
	if !ValidEncryptionRoot(target.Dataset) {
		return errors.New("invalid protected dataset")
	}
	if target.GUID != "" {
		guid, err := s.property(ctx, target.Dataset, "guid")
		if err != nil || guid != target.GUID {
			return errors.New("ZFS identity changed")
		}
	}
	root, err := s.property(ctx, target.Dataset, "encryptionroot")
	if err != nil || root != target.Dataset {
		return errors.New("encryption root mismatch")
	}
	output, err := s.Run(ctx, "zfs", []string{"list", "-H", "-t", "filesystem", "-o", "name", "-r", target.Dataset}, nil)
	if err != nil {
		return errors.New("cannot enumerate protected filesystems")
	}
	names := strings.Fields(string(output))
	if len(names) == 0 {
		return errors.New("protected filesystems missing")
	}
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		if name != target.Dataset && !strings.HasPrefix(name, target.Dataset+"/") {
			return errors.New("unexpected ZFS descendant")
		}
		mounted, err := s.property(ctx, name, "mounted")
		if err != nil {
			return err
		}
		if mounted == "yes" {
			_, err = s.Run(ctx, "zfs", []string{"unmount", name}, nil)
			if err != nil {
				return errors.New("protected filesystem remains busy")
			}
		}
	}
	status, err := s.property(ctx, target.Dataset, "keystatus")
	if err != nil {
		return err
	}
	if status != "unavailable" {
		_, err = s.Run(ctx, "zfs", []string{"unload-key", "-r", target.Dataset}, nil)
		if err != nil {
			return errors.New("ZFS key remains in use")
		}
	}
	status, err = s.property(ctx, target.Dataset, "keystatus")
	if err != nil || status != "unavailable" {
		return errors.New("ZFS key unload verification failed")
	}
	return nil
}

// EncryptedDataset describes discoverable storage without returning key material.
type EncryptedDataset struct {
	Dataset        string `json:"dataset"`
	EncryptionRoot string `json:"encryptionRoot"`
	Algorithm      string `json:"algorithm"`
	KeyStatus      string `json:"keyStatus"`
	KeyFormat      string `json:"keyFormat"`
}

// Inventory includes external encryption roots, even before they are registered with the node.
func (s *Storage) Inventory(ctx context.Context) ([]EncryptedDataset, error) {
	output, err := s.Run(ctx, "zfs", []string{"list", "-H", "-t", "filesystem,volume", "-o", "name,encryption,encryptionroot,keystatus,keyformat"}, nil)
	if err != nil {
		return nil, errors.New("cannot inspect ZFS encryption state")
	}
	items := []EncryptedDataset{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 {
			return nil, errors.New("invalid ZFS encryption inventory")
		}
		if fields[1] != "off" && fields[1] != "-" && fields[3] != "available" && fields[3] != "unavailable" {
			return nil, errors.New("invalid ZFS key status")
		}
		items = append(items, EncryptedDataset{Dataset: fields[0], Algorithm: fields[1], EncryptionRoot: fields[2], KeyStatus: fields[3], KeyFormat: fields[4]})
	}
	return items, nil
}
