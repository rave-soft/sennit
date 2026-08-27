package fsext

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type pathLock struct {
	mutex sync.Mutex
	users int
}

var conditionalReplaceState = struct {
	sync.Mutex
	locks       map[string]*pathLock
	beforeCheck func(string)
	readFile    func(string) ([]byte, error)
	rename      func(string, string) error
}{
	locks:       make(map[string]*pathLock),
	beforeCheck: func(string) {},
	readFile:    os.ReadFile,
	rename:      os.Rename,
}

func lockMutationPath(path string) func() {
	conditionalReplaceState.Lock()
	lock := conditionalReplaceState.locks[path]
	if lock == nil {
		lock = &pathLock{}
		conditionalReplaceState.locks[path] = lock
	}
	lock.users++
	conditionalReplaceState.Unlock()

	lock.mutex.Lock()
	return func() {
		lock.mutex.Unlock()
		conditionalReplaceState.Lock()
		lock.users--
		if lock.users == 0 {
			delete(conditionalReplaceState.locks, path)
		}
		conditionalReplaceState.Unlock()
	}
}

func conditionalReplaceExisting(path string, expected, data []byte, mode os.FileMode) error {
	path = filepath.Clean(path)
	unlock := lockMutationPath(path)
	defer unlock()

	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tempPath)
	}
	defer cleanup()

	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}

	conditionalReplaceState.Lock()
	beforeCheck := conditionalReplaceState.beforeCheck
	readFile := conditionalReplaceState.readFile
	rename := conditionalReplaceState.rename
	conditionalReplaceState.Unlock()
	beforeCheck(path)

	current, err := readFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrFileChanged
	}
	if err != nil {
		return fmt.Errorf("read destination before replacement: %w", err)
	}
	if !bytes.Equal(current, expected) {
		return ErrFileChanged
	}
	if err := rename(tempPath, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}
