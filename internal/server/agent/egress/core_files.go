package egress

import (
	"bytes"
	"context"
	"os"
)

// Stage only when configuration, service definition or runtime identity requires a restart.
func stageCoreFiles(ctx context.Context, binary, configPath, unitPath string, body []byte, unit string, reset bool) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	current, _ := os.ReadFile(configPath)
	oldUnit, _ := os.ReadFile(unitPath)
	if !reset && bytes.Equal(current, body) && string(oldUnit) == unit {
		return false, nil
	}
	next := configPath + ".next"
	{
		err := atomicFile(next, body, 0o600)
		if err != nil {
			return false, err
		}
	}
	defer func() { _ = os.Remove(next) }()
	{
		err := validateCore(ctx, binary, next)
		if err != nil {
			return false, err
		}
	}
	{
		err := os.Rename(next, configPath)
		if err != nil {
			return false, err
		}
	}
	{
		err := atomicFile(unitPath, []byte(unit), 0o644)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}
