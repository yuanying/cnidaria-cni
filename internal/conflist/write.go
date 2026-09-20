package conflist

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile puts data at path and reports whether the file changed. Identical
// content is left untouched, so a reconcile that recomputes the same list does not
// make the container runtime reload it. New content goes to a temporary file in the
// same directory and is renamed over the old one, so a reader sees either the whole
// old list or the whole new one.
func WriteFile(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return false, fmt.Errorf("conflist: %w", err)
	}
	if err := fill(tmp, data); err != nil {
		return false, errors.Join(fmt.Errorf("conflist: write %s: %w", tmp.Name(), err), os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, errors.Join(fmt.Errorf("conflist: %w", err), os.Remove(tmp.Name()))
	}
	return true, nil
}

// fill writes data, makes the file world-readable as a file in /etc/cni/net.d is
// expected to be, and closes it, reporting the first thing that went wrong.
func fill(f *os.File, data []byte) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	return errors.Join(err, f.Close())
}
