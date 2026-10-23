package egress

import "testing"

func mustValue[T any](t *testing.T, value any) T {
	t.Helper()
	typed, ok := value.(T)
	if !ok {
		t.Fatalf("unexpected JSON type %T", value)
	}

	return typed
}
