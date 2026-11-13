// Package backups runs node-owned instance exports independently of HTTP clients.
package backups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const Extension = "incus_instance_backup_v1"
const TransferExtension = "incus_instance_transfer_v1"

var instanceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,62}$`)

type Store interface {
	Check() error
	LoadBackupState() ([]byte, error)
	SaveBackupState([]byte) error
}

type Item struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	File    string `json:"file,omitempty"`
	Bytes   int64  `json:"bytes"`
	Error   string `json:"error,omitempty"`
}

type Task struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind,omitempty"`
	Instance   string     `json:"instance,omitempty"`
	FilePath   string     `json:"filePath,omitempty"`
	Directory  string     `json:"directory"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Items      []Item     `json:"items"`
	Error      string     `json:"error,omitempty"`
}

type Source interface {
	Inventory(context.Context, string) ([]Item, error)
	Export(context.Context, Item, io.WriteSeeker) error
}

type ImportSource interface {
	Import(context.Context, io.Reader, string, string) (string, error)
}

type Manager struct {
	mu     sync.Mutex
	ctx    context.Context
	store  Store
	source Source
	tasks  []Task
	err    error
}

func New(ctx context.Context, store Store) *Manager {
	return newManager(ctx, store, nativeSource{})
}

func newManager(ctx context.Context, store Store, source Source) *Manager {
	m := &Manager{ctx: ctx, store: store, source: source, tasks: []Task{}}
	data, err := store.LoadBackupState()
	if err == nil && len(data) > 0 {
		err = json.Unmarshal(data, &m.tasks)
	}
	clear(data)
	if err != nil {
		m.err = fmt.Errorf("backup journal unavailable: %w", err)
		return m
	}
	changed := false
	for i := range m.tasks {
		if m.tasks[i].Status == "running" {
			now := time.Now().UTC()
			m.tasks[i].Status = "interrupted"
			m.tasks[i].Error = "Incus restarted; completed archives are preserved; start a new backup"
			m.tasks[i].FinishedAt = &now
			for j := range m.tasks[i].Items {
				if m.tasks[i].Items[j].Status == "running" || m.tasks[i].Items[j].Status == "pending" {
					m.tasks[i].Items[j].Status = "interrupted"
				}
			}
			changed = true
		}
	}
	if changed {
		m.err = m.save()
	}
	return m
}

// save is called with the manager lock held, except during construction.
func (m *Manager) save() error {
	data, err := json.Marshal(m.tasks)
	if err != nil {
		return err
	}
	return m.store.SaveBackupState(data)
}

func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	result := map[string]any{"type": "sync", "metadata": value, "status_code": 200}
	if status != 200 {
		result = map[string]any{"type": "error", "error": fmt.Sprint(value), "error_code": status}
	}
	_ = json.NewEncoder(w).Encode(result)
}

// ServeHTTP is mounted behind Incus server can_edit authorization.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		reply(w, 503, m.err.Error())
		return
	}
	if r.Method == http.MethodGet {
		reply(w, 200, map[string]any{"tasks": m.tasks})
		return
	}
	if r.Method != http.MethodPost {
		reply(w, 405, "method not allowed")
		return
	}
	var input struct {
		ID        string `json:"id"`
		Directory string `json:"directory"`
		Instance  string `json:"instance"`
		FilePath  string `json:"filePath"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8193))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(new(any)) != io.EOF || len(input.ID) != 32 {
		reply(w, 400, "invalid backup request")
		return
	}
	if _, err := hex.DecodeString(input.ID); err != nil || input.ID != strings.ToLower(input.ID) {
		reply(w, 400, "invalid backup id")
		return
	}
	if input.Instance != "" && !instanceNamePattern.MatchString(input.Instance) {
		reply(w, 400, "invalid instance name")
		return
	}
	if input.FilePath != "" && input.Instance == "" {
		reply(w, 400, "filePath requires instance")
		return
	}

	var directory string
	var taskDirectory string
	var targetFilePath string
	if input.FilePath != "" {
		resolvedFile, err := validateTargetFilePath(input.FilePath)
		if err != nil {
			reply(w, 400, err.Error())
			return
		}
		if _, statErr := os.Lstat(resolvedFile); statErr == nil {
			reply(w, 409, "target backup file already exists")
			return
		} else if !errors.Is(statErr, os.ErrNotExist) {
			reply(w, 400, "cannot access target backup file")
			return
		}
		directory = filepath.Dir(resolvedFile)
		taskDirectory = directory
		targetFilePath = resolvedFile
	} else {
		var err error
		directory, err = validateDirectory(input.Directory)
		if err != nil {
			reply(w, 400, err.Error())
			return
		}
		if input.Instance != "" {
			taskDirectory = directory
			targetFilePath = filepath.Join(directory, fmt.Sprintf("%s-%s.tar.gz", input.Instance, input.ID[:8]))
			if _, statErr := os.Lstat(targetFilePath); statErr == nil {
				reply(w, 409, "target backup file already exists")
				return
			}
		} else {
			taskDirectory = filepath.Join(directory, "instances-"+input.ID)
		}
	}

	for _, task := range m.tasks {
		if task.ID == input.ID {
			// A retry must identify the same operation, including its destination.
			expectedDir := filepath.Dir(task.Directory)
			if task.FilePath != "" {
				expectedDir = filepath.Dir(task.FilePath)
			}
			if directory != expectedDir || task.Instance != input.Instance {
				reply(w, 409, "backup id already used for another destination")
				return
			}
			reply(w, 200, task)
			return
		}
	}
	for _, task := range m.tasks {
		if task.Status == "running" {
			reply(w, 409, "a node backup is already running")
			return
		}
	}
	if err := m.store.Check(); err != nil {
		reply(w, 409, "node is locked or security state unavailable")
		return
	}
	// Inventory and export run after durable acceptance, using the daemon context.
	task := Task{
		ID:        input.ID,
		Kind:      "export",
		Instance:  input.Instance,
		FilePath:  targetFilePath,
		Directory: taskDirectory,
		Status:    "running",
		CreatedAt: time.Now().UTC(),
		Items:     []Item{},
	}
	old := append([]Task(nil), m.tasks...)
	m.tasks = append([]Task{task}, m.tasks...)
	if len(m.tasks) > 50 {
		m.tasks = m.tasks[:50]
	}
	if err := m.save(); err != nil {
		m.tasks = old
		reply(w, 503, "cannot persist backup task")
		return
	}
	go m.run(input.ID)
	reply(w, 200, task)
}

func validateDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("backup location must be an absolute host directory")
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("backup directory must already exist and be accessible")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", errors.New("backup location is not a directory")
	}
	root := os.Getenv("INCUS_DIR")
	if root == "" {
		root = "/var/lib/incus"
	}
	for _, forbidden := range []string{"/", "/proc", "/sys", "/dev", "/etc", "/usr", "/bin", "/sbin", "/run", "/boot", root} {
		canonical := forbidden
		if actual, resolveErr := filepath.EvalSymlinks(forbidden); resolveErr == nil {
			canonical = actual
		}
		if path == forbidden || resolved == canonical || (forbidden != "/" && (strings.HasPrefix(path, forbidden+"/") || strings.HasPrefix(resolved, canonical+"/"))) {
			return "", errors.New("backup location must be outside system and Incus data directories")
		}
	}
	return resolved, nil
}

func validateTargetFilePath(path string) (string, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return "", errors.New("backup file path must be an absolute host path")
	}
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	if base == "" || base == "." || base == ".." || base == "/" {
		return "", errors.New("backup file path must include a file name")
	}
	parent, err := validateDirectory(filepath.Dir(clean))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, base), nil
}

func validateSourceFilePath(path string) (string, int64, error) {
	if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\x00\r\n") {
		return "", 0, errors.New("import file path must be an absolute host path")
	}
	clean := filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", 0, errors.New("import file does not exist or is not accessible")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", 0, errors.New("import path must be a regular file")
	}
	if info.Size() == 0 {
		return "", 0, errors.New("import file is empty")
	}
	if _, err := validateDirectory(filepath.Dir(resolved)); err != nil {
		return "", 0, err
	}
	return resolved, info.Size(), nil
}

// ServeImportHTTP handles POST /1.0/agent/instance-import to restore an archive from a host file path.
func (m *Manager) ServeImportHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		reply(w, 503, m.err.Error())
		return
	}
	if r.Method != http.MethodPost {
		reply(w, 405, "method not allowed")
		return
	}
	var input struct {
		ID          string `json:"id"`
		FilePath    string `json:"filePath"`
		Name        string `json:"name"`
		StoragePool string `json:"storagePool"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 8193))
	dec.DisallowUnknownFields()
	if dec.Decode(&input) != nil || dec.Decode(new(any)) != io.EOF || len(input.ID) != 32 {
		reply(w, 400, "invalid import request")
		return
	}
	if _, err := hex.DecodeString(input.ID); err != nil || input.ID != strings.ToLower(input.ID) {
		reply(w, 400, "invalid import id")
		return
	}
	if input.Name != "" && !instanceNamePattern.MatchString(input.Name) {
		reply(w, 400, "invalid instance name")
		return
	}
	resolvedFile, fileSize, err := validateSourceFilePath(input.FilePath)
	if err != nil {
		reply(w, 400, err.Error())
		return
	}
	for _, task := range m.tasks {
		if task.ID == input.ID {
			if task.Kind != "import" || task.FilePath != resolvedFile {
				reply(w, 409, "import id already used for another file")
				return
			}
			reply(w, 200, task)
			return
		}
	}
	for _, task := range m.tasks {
		if task.Status == "running" {
			reply(w, 409, "a node transfer task is already running")
			return
		}
	}
	if err := m.store.Check(); err != nil {
		reply(w, 409, "node is locked or security state unavailable")
		return
	}
	task := Task{
		ID:        input.ID,
		Kind:      "import",
		Instance:  input.Name,
		FilePath:  resolvedFile,
		Directory: filepath.Dir(resolvedFile),
		Status:    "running",
		CreatedAt: time.Now().UTC(),
		Items: []Item{{
			Project: "default",
			Name:    input.Name,
			Status:  "running",
			File:    filepath.Base(resolvedFile),
			Bytes:   fileSize,
		}},
	}
	old := append([]Task(nil), m.tasks...)
	m.tasks = append([]Task{task}, m.tasks...)
	if len(m.tasks) > 50 {
		m.tasks = m.tasks[:50]
	}
	if err := m.save(); err != nil {
		m.tasks = old
		reply(w, 503, "cannot persist import task")
		return
	}
	go m.runImport(input.ID, resolvedFile, input.Name, input.StoragePool)
	reply(w, 200, task)
}

func (m *Manager) runImport(id, filePath, name, storagePool string) {
	var runErr error
	var importedName string
	defer func() {
		if recovered := recover(); recovered != nil {
			runErr = fmt.Errorf("import worker failed: %v", recovered)
		}
		_ = m.update(id, func(t *Task) {
			now := time.Now().UTC()
			t.FinishedAt = &now
			if runErr != nil {
				t.Status = "failed"
				if m.ctx.Err() != nil {
					t.Status = "interrupted"
				}
				t.Error = runErr.Error()
				if len(t.Items) > 0 {
					t.Items[0].Status = "failed"
					t.Items[0].Error = runErr.Error()
				}
				return
			}
			t.Status = "success"
			if importedName != "" {
				t.Instance = importedName
				if len(t.Items) > 0 {
					t.Items[0].Name = importedName
				}
			}
			if len(t.Items) > 0 {
				t.Items[0].Status = "success"
			}
		})
	}()
	if err := m.ctx.Err(); err != nil {
		runErr = err
		return
	}
	if err := m.store.Check(); err != nil {
		runErr = errors.New("node is locked during import")
		return
	}
	imp, ok := m.source.(ImportSource)
	if !ok {
		runErr = errors.New("instance import is not supported by backup source")
		return
	}
	file, err := os.Open(filePath)
	if err != nil {
		runErr = err
		return
	}
	defer file.Close()
	importedName, runErr = imp.Import(m.ctx, file, name, storagePool)
}

func (m *Manager) update(id string, change func(*Task)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.tasks {
		if m.tasks[i].ID == id {
			change(&m.tasks[i])
			if err := m.save(); err != nil {
				m.err = err
				return err
			}
			return nil
		}
	}
	return errors.New("backup task not found")
}

func (m *Manager) run(id string) {
	var runErr error
	defer func() {
		if recovered := recover(); recovered != nil {
			runErr = fmt.Errorf("backup worker failed: %v", recovered)
		}
		_ = m.update(id, func(t *Task) {
			now := time.Now().UTC()
			t.FinishedAt = &now
			t.Status = "success"
			failed := 0
			for i := range t.Items {
				if runErr != nil && (t.Items[i].Status == "pending" || t.Items[i].Status == "running") {
					t.Items[i].Status = "interrupted"
				}
				if t.Items[i].Status != "success" {
					failed++
				}
			}
			if runErr != nil {
				t.Status = "failed"
				if m.ctx.Err() != nil {
					t.Status = "interrupted"
				}
				t.Error = runErr.Error()
			} else if failed > 0 {
				t.Status = "partial"
				var detail string
				for _, item := range t.Items {
					if item.Status != "success" && item.Error != "" {
						detail = fmt.Sprintf("%s: %s", item.Name, item.Error)
						break
					}
				}
				if detail != "" {
					t.Error = fmt.Sprintf("%d instance exports failed (%s)", failed, detail)
				} else {
					t.Error = fmt.Sprintf("%d instance exports failed", failed)
				}
			}
		})
	}()
	var task Task
	if err := m.update(id, func(t *Task) { task = *t }); err != nil {
		runErr = err
		return
	}
	runErr = m.execute(task)
}

func (m *Manager) execute(task Task) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	items, err := m.source.Inventory(m.ctx, task.Directory)
	if err != nil {
		return err
	}
	if task.Instance != "" {
		filtered := make([]Item, 0, 1)
		for _, item := range items {
			if item.Name == task.Instance {
				filtered = append(filtered, item)
				break
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("instance %q not found", task.Instance)
		}
		items = filtered
	}
	if err = m.update(task.ID, func(t *Task) { t.Items = items }); err != nil {
		return err
	}
	if task.Instance == "" {
		if err = os.Mkdir(task.Directory, 0o700); err != nil {
			return err
		}
	}
	dir, err := os.OpenRoot(task.Directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	for i, item := range items {
		if err = m.store.Check(); err != nil {
			return errors.New("node locked during backup; completed archives are preserved")
		}
		if err = m.ctx.Err(); err != nil {
			return err
		}
		filename := fmt.Sprintf("%04d-%s.tar.gz", i+1, url.PathEscape(item.Project)+"_"+url.PathEscape(item.Name))
		if task.Instance != "" && task.FilePath != "" {
			filename = filepath.Base(task.FilePath)
		}
		if err = m.update(task.ID, func(t *Task) { t.Items[i].Status = "running" }); err != nil {
			return err
		}
		exportErr := func() error {
			file, openErr := dir.OpenFile(filename+".partial", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if openErr != nil {
				return openErr
			}
			defer file.Close()
			exportErr := m.source.Export(m.ctx, item, file)
			if exportErr == nil {
				exportErr = file.Sync()
			}
			info, statErr := file.Stat()
			closeErr := file.Close()
			if exportErr == nil {
				exportErr = statErr
			}
			if exportErr == nil {
				exportErr = closeErr
			}
			if exportErr == nil && info.Size() == 0 {
				exportErr = errors.New("node returned an empty backup archive")
			}
			if exportErr == nil {
				exportErr = dir.Rename(filename+".partial", filename)
			}
			if exportErr == nil {
				directoryFile, syncErr := dir.Open(".")
				if syncErr == nil {
					syncErr = directoryFile.Sync()
					_ = directoryFile.Close()
				}
				exportErr = syncErr
			}
			if exportErr == nil {
				return m.update(task.ID, func(t *Task) {
					t.Items[i].Status = "success"
					t.Items[i].File = filename
					t.Items[i].Bytes = info.Size()
				})
			}
			_ = dir.Remove(filename + ".partial")
			return exportErr
		}()
		if exportErr != nil {
			if err = m.update(task.ID, func(t *Task) { t.Items[i].Status = "failed"; t.Items[i].Error = exportErr.Error() }); err != nil {
				return err
			}
		}
	}
	return nil
}

func randomName() string {
	var value [8]byte
	_, _ = rand.Read(value[:])
	return hex.EncodeToString(value[:])
}
