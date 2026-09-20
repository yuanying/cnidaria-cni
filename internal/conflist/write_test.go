package conflist

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "10-cnidaria.conflist")
	changed, err := WriteFile(path, []byte("one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("WriteFile reported no change on a new file")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "one\n" {
		t.Errorf("file holds %q, want %q", got, "one\n")
	}
}

// The runtime watches /etc/cni/net.d, so a rewrite that changes nothing is noise at
// best and a spurious reload at worst. Same content leaves the file alone.
func TestWriteFileLeavesAnUnchangedFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "10-cnidaria.conflist")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := WriteFile(path, []byte("one\n"))
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("WriteFile reported a change for identical content")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("the file was rewritten although its content did not change")
	}
}

func TestWriteFileReplacesChangedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "10-cnidaria.conflist")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteFile(path, []byte("two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("WriteFile reported no change for new content")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "two\n" {
		t.Errorf("file holds %q, want %q", got, "two\n")
	}
}

// The file is written next to its final name and renamed into place, so the runtime
// never reads a half-written list, and nothing is left behind in the directory.
func TestWriteFileLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteFile(filepath.Join(dir, "10-cnidaria.conflist"), []byte("one\n")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "10-cnidaria.conflist" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the conflist", names)
	}
}

func TestWriteFileFailsWhenTheDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "10-cnidaria.conflist")
	if _, err := WriteFile(path, []byte("one\n")); err == nil {
		t.Error("WriteFile returned nil for a directory that does not exist")
	}
}
