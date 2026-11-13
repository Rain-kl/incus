//go:build linux

package security

import (
	"os"
	"path/filepath"
)

// LoadBackupState authenticates the backup journal with the node state key.
func (m *Manager) LoadBackupState() ([]byte, error) {
	var data []byte
	err := m.transact(false, func(_ *State) error {
		var err error
		data, err = m.readSealed(filepath.Join(m.dir, "instance-backups"))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
	return data, err
}

// SaveBackupState atomically persists the backup journal without storing plaintext.
func (m *Manager) SaveBackupState(data []byte) error {
	return m.transact(true, func(_ *State) error {
		return m.writeSealed(filepath.Join(m.dir, "instance-backups"), data)
	})
}
