package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/rave-soft/sennit/internal/config"
	"github.com/rave-soft/sennit/internal/lock"
)

const projectsFileName = "projects.json"

// projectsLockDeadline bounds how long Register waits for the cross-process
// flock on projects.json before giving up.
const projectsLockDeadline = 5 * time.Second

// Project represents a tracked project directory.
type Project struct {
	Path         string    `json:"path"`
	DataDir      string    `json:"data_dir"`
	LastAccessed time.Time `json:"last_accessed"`
}

// ProjectList holds the list of tracked projects.
type ProjectList struct {
	Projects []Project `json:"projects"`
}

var mu sync.Mutex

// projectsFilePath returns the path to the projects.json file.
func projectsFilePath() string {
	return filepath.Join(filepath.Dir(config.GlobalConfigData()), projectsFileName)
}

// Load reads the projects list from disk.
func Load() (*ProjectList, error) {
	mu.Lock()
	defer mu.Unlock()

	return loadLocked()
}

// loadLocked reads the projects list from disk. Callers must hold mu.
func loadLocked() (*ProjectList, error) {
	path := projectsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &ProjectList{Projects: []Project{}}, nil
		}
		return nil, err
	}

	var list ProjectList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}

	return &list, nil
}

// saveLocked writes the projects list to disk atomically (temp file +
// rename), so a crash mid-write never leaves a truncated projects.json.
// Callers must hold mu.
func saveLocked(list *ProjectList) error {
	path := projectsFilePath()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}

	return atomicWriteFile(path, data, 0o600)
}

// Register adds or updates a project in the list. The read-modify-write
// cycle is protected by an in-process mutex (mu) against concurrent
// goroutines in this process, and by a cross-process flock on
// projects.json.lock against concurrent Braid processes — otherwise two
// processes registering at the same time can race and one update is lost.
func Register(workingDir, dataDir string) error {
	mu.Lock()
	defer mu.Unlock()

	path := projectsFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), projectsLockDeadline)
	defer cancel()
	release, err := lock.File(ctx, path+".lock")
	if err != nil {
		return fmt.Errorf("acquire projects lock: %w", err)
	}
	defer release()

	list, err := loadLocked()
	if err != nil {
		return err
	}

	now := time.Now().UTC()

	// Check if project already exists
	found := false
	for i, p := range list.Projects {
		if p.Path == workingDir {
			list.Projects[i].DataDir = dataDir
			list.Projects[i].LastAccessed = now
			found = true
			break
		}
	}

	if !found {
		list.Projects = append(list.Projects, Project{
			Path:         workingDir,
			DataDir:      dataDir,
			LastAccessed: now,
		})
	}

	// Sort by last accessed (most recent first)
	slices.SortFunc(list.Projects, func(a, b Project) int {
		if a.LastAccessed.After(b.LastAccessed) {
			return -1
		}
		if a.LastAccessed.Before(b.LastAccessed) {
			return 1
		}
		return 0
	})

	return saveLocked(list)
}

// atomicWriteFile writes data to a file atomically by writing to a unique
// temporary file in the same directory and renaming it into place. This
// prevents concurrent readers (or a crash mid-write) from observing a
// partially-written projects.json.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the write error above is what matters
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		_ = os.Remove(tmp) // best-effort cleanup; the chmod error above is what matters
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the close error above is what matters
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; the rename error above is what matters
		return err
	}
	return nil
}

// List returns all tracked projects sorted by last accessed.
func List() ([]Project, error) {
	list, err := Load()
	if err != nil {
		return nil, err
	}
	return list.Projects, nil
}
