package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

func IsTrusted(workingDir string) bool {
	info, err := os.Lstat(trustPath(workingDir))
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o077 == 0
}

func Trust(workingDir string) error {
	path := trustPath(workingDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create trusted-projects directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secure trusted-projects directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) && IsTrusted(workingDir) {
			return nil
		}
		return fmt.Errorf("create project trust marker: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close project trust marker: %w", err)
	}
	return nil
}

func trustPath(workingDir string) string {
	identity, err := filepath.Abs(workingDir)
	if err != nil {
		identity = workingDir
	}
	identity = filepath.Clean(identity)
	if canonical, err := filepath.EvalSymlinks(identity); err == nil {
		identity = canonical
	}
	sum := sha256.Sum256([]byte(identity))
	return filepath.Join(filepath.Dir(GlobalConfig()), "trusted-projects", hex.EncodeToString(sum[:]))
}
