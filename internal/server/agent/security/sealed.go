//go:build linux

package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

var errStateUnavailable = errors.New("node state unavailable; refusing operation")

// stateKey atomically installs a random key; existing records prohibit regenerating a lost key.
func (m *Manager) stateKey(create bool) ([]byte, error) {
	path := filepath.Join(m.dir, "seed")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == nil {
		f := os.NewFile(uintptr(fd), path)
		defer f.Close()
		info, statErr := f.Stat()
		if statErr != nil {
			return nil, errStateUnavailable
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Geteuid()) || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return nil, errStateUnavailable
		}
		key, readErr := io.ReadAll(io.LimitReader(f, 33))
		if readErr != nil || len(key) != 32 {
			clear(key)
			return nil, errStateUnavailable
		}
		return key, nil
	}
	if !create || !os.IsNotExist(err) {
		return nil, errStateUnavailable
	}
	if _, err = os.Lstat(filepath.Join(m.dir, "index")); !os.IsNotExist(err) {
		return nil, errStateUnavailable
	}
	records, err := os.ReadDir(filepath.Join(m.dir, "queue"))
	if err != nil && !os.IsNotExist(err) {
		return nil, errStateUnavailable
	}
	if len(records) > 0 {
		return nil, errStateUnavailable
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return nil, err
	}
	defer clear(key)
	tmp, err := os.CreateTemp(m.dir, ".seed-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(key)
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	// Link publishes the complete key without replacing a concurrent writer's key.
	if err = os.Link(tmp.Name(), path); err != nil && !os.IsExist(err) {
		return nil, err
	}
	if err = syncDirectory(m.dir); err != nil {
		return nil, err
	}
	return m.stateKey(false)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (m *Manager) stateCipher(create bool) (cipher.AEAD, error) {
	key, err := m.stateKey(create)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// writeSealed binds ciphertext to its record name, preventing state/event substitution.
func (m *Manager) writeSealed(path string, data []byte) error {
	defer clear(data)
	aead, err := m.stateCipher(true)
	if err != nil {
		return err
	}
	header := make([]byte, 1+aead.NonceSize())
	header[0] = 1
	if _, err = rand.Read(header[1:]); err != nil {
		return err
	}
	sealed := aead.Seal(header, header[1:], data, []byte(filepath.Base(path)))
	return atomicWrite(path, sealed)
}

func (m *Manager) readSealed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	aead, err := m.stateCipher(false)
	if err != nil {
		return nil, errStateUnavailable
	}
	n := 1 + aead.NonceSize()
	if len(data) < n+aead.Overhead() || data[0] != 1 {
		return nil, errStateUnavailable
	}
	plain, err := aead.Open(nil, data[1:n], data[n:], []byte(filepath.Base(path)))
	if err != nil {
		return nil, errStateUnavailable
	}
	return plain, nil
}
