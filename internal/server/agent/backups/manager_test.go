package backups

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu     sync.Mutex
	data   []byte
	locked bool
	fail   bool
}

func (s *memoryStore) Check() error {
	if s.locked {
		return errors.New("locked")
	}
	return nil
}
func (s *memoryStore) LoadBackupState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...), nil
}
func (s *memoryStore) SaveBackupState(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("unavailable")
	}
	s.data = append([]byte(nil), data...)
	return nil
}

type fixtureSource struct {
	entered chan struct{}
	release chan struct{}
}

func (s fixtureSource) Inventory(ctx context.Context, _ string) ([]Item, error) {
	if _, has := ctx.Deadline(); has {
		return nil, errors.New("unexpected deadline")
	}
	return []Item{{Project: "default", Name: "good", Status: "pending"}, {Project: "other", Name: "bad", Status: "pending"}}, nil
}
func (s fixtureSource) Export(ctx context.Context, item Item, w io.WriteSeeker) error {
	if item.Name == "bad" {
		return errors.New("export failed")
	}
	if s.entered != nil {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, err := w.Write([]byte("fixture archive"))
	return err
}
func (s fixtureSource) Import(_ context.Context, r io.Reader, name string, _ string) (string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if string(data) != "fixture archive" {
		return "", errors.New("unexpected archive payload")
	}
	if name != "" {
		return name, nil
	}
	return "imported-from-archive", nil
}
func post(m *Manager, id, path string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"id": id, "directory": path})
	r := httptest.NewRequest("POST", "/1.0/agent/instance-backups", strings.NewReader(string(body)))
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r.WithContext(ctx))
	return w
}
func awaitTask(t *testing.T, m *Manager) Task {
	t.Helper()
	for range 500 {
		m.mu.Lock()
		task := m.tasks[0]
		done := task.Status != "running"
		m.mu.Unlock()
		if done {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Task{}
}
func TestDetachedExportIdempotencyAndPartialFailure(t *testing.T) {
	store := &memoryStore{}
	source := fixtureSource{entered: make(chan struct{}), release: make(chan struct{})}
	m := newManager(context.Background(), store, source)
	path := t.TempDir()
	id := strings.Repeat("a", 32)
	if w := post(m, id, path); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	<-source.entered
	if w := post(m, id, path); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := post(m, strings.Repeat("b", 32), path); w.Code != 409 {
		t.Fatal(w.Code)
	}
	close(source.release)
	task := awaitTask(t, m)
	if task.Status != "partial" || task.Items[0].Status != "success" || task.Items[1].Status != "failed" {
		t.Fatalf("%+v", task)
	}
	data, err := os.ReadFile(filepath.Join(task.Directory, task.Items[0].File))
	if err != nil || string(data) != "fixture archive" {
		t.Fatal(err)
	}
	files, err := os.ReadDir(task.Directory)
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	if w := post(m, id, path); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if len(m.tasks) != 1 {
		t.Fatal("replayed task duplicated")
	}
	restored := newManager(context.Background(), store, fixtureSource{})
	if len(restored.tasks) != 1 || restored.tasks[0].Status != "partial" {
		t.Fatal(restored.tasks)
	}
}
func TestRejectedRequestsNeverStart(t *testing.T) {
	for _, tc := range []struct {
		path         string
		locked, fail bool
		code         int
	}{{"relative", false, false, 400}, {"/", false, false, 400}, {"/etc", false, false, 400}, {"/does-not-exist", false, false, 400}, {"", true, false, 409}, {"", false, true, 503}} {
		path := tc.path
		if path == "" {
			path = t.TempDir()
		}
		m := newManager(context.Background(), &memoryStore{locked: tc.locked, fail: tc.fail}, fixtureSource{})
		if w := post(m, strings.Repeat("a", 32), path); w.Code != tc.code {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
		if len(m.tasks) != 0 {
			t.Fatal("rejected job retained")
		}
	}
}
func TestRestartMarksOnlyUnfinishedJobsInterrupted(t *testing.T) {
	store := &memoryStore{}
	data, _ := json.Marshal([]Task{{ID: "old", Status: "success"}, {ID: "new", Status: "running", Items: []Item{{Status: "success"}, {Status: "running"}, {Status: "pending"}}}})
	_ = store.SaveBackupState(data)
	m := newManager(context.Background(), store, fixtureSource{})
	if m.err != nil || m.tasks[0].Status != "success" || m.tasks[1].Status != "interrupted" || m.tasks[1].Items[0].Status != "success" || m.tasks[1].Items[1].Status != "interrupted" {
		t.Fatal(m.tasks, m.err)
	}
}
func TestSymlinkCannotSelectSystemDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "escape")
	if err := os.Symlink("/etc", path); err != nil {
		t.Fatal(err)
	}
	if _, err := validateDirectory(path); err == nil {
		t.Fatal("system directory accepted")
	}
}
func TestSingleInstanceExportAndImport(t *testing.T) {
	store := &memoryStore{}
	m := newManager(context.Background(), store, fixtureSource{})
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "good-export.tar.gz")
	body, _ := json.Marshal(map[string]string{
		"id":       strings.Repeat("d", 32),
		"instance": "good",
		"filePath": targetFile,
	})
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest("POST", "/1.0/agent/instance-backups", strings.NewReader(string(body))))
	if w.Code != 200 {
		t.Fatalf("export status=%d body=%s", w.Code, w.Body.String())
	}
	task := awaitTask(t, m)
	if task.Status != "success" || task.Kind != "export" || task.Instance != "good" || len(task.Items) != 1 || task.FilePath == "" {
		t.Fatalf("unexpected export task: %+v", task)
	}
	exported, err := os.ReadFile(task.FilePath)
	if err != nil || string(exported) != "fixture archive" {
		t.Fatalf("read exported file: err=%v data=%q", err, string(exported))
	}

	importBody, _ := json.Marshal(map[string]string{
		"id":       strings.Repeat("e", 32),
		"filePath": task.FilePath,
	})
	iw := httptest.NewRecorder()
	m.ServeImportHTTP(iw, httptest.NewRequest("POST", "/1.0/agent/instance-import", strings.NewReader(string(importBody))))
	if iw.Code != 200 {
		t.Fatalf("import status=%d body=%s", iw.Code, iw.Body.String())
	}
	impTask := awaitTask(t, m)
	if impTask.Status != "success" || impTask.Kind != "import" || impTask.Instance != "imported-from-archive" || impTask.Items[0].Name != "imported-from-archive" {
		t.Fatalf("unexpected import task: %+v", impTask)
	}
}
