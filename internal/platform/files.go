package platform

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

func ReadProtected(path, fallback string) (string, error) {
	if path == "" {
		return fallback, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("secret file could not be read: %s", filepath.Base(path))
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("secret file is not a protected regular file: %s", filepath.Base(path))
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("secret file could not be read: %s", filepath.Base(path))
	}
	return stringTrimSpace(string(value)), nil
}

func WriteExclusive(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err = file.Write(content); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	return closeErr
}

func AtomicCopy(source, destination string, mode os.FileMode) error {
	sourceInfo, sourceErr := os.Stat(source)
	if destinationInfo, err := os.Stat(destination); sourceErr == nil && err == nil && os.SameFile(sourceInfo, destinationInfo) {
		return errors.New("source and destination must differ")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(destination), "."+filepath.Base(destination)+fmt.Sprintf(".%d.tmp", os.Getpid()))
	output, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer os.Remove(temporary)
	_, copyErr := io.Copy(output, input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	if closeErr := output.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(destination))
}

func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func RequireProtectedRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("file must be a protected regular file")
	}
	return nil
}

func stringTrimSpace(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\n' || value[0] == '\r' || value[0] == '\t') {
		value = value[1:]
	}
	for len(value) > 0 {
		last := value[len(value)-1]
		if last != ' ' && last != '\n' && last != '\r' && last != '\t' {
			break
		}
		value = value[:len(value)-1]
	}
	return value
}
