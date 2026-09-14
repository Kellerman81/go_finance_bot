package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureFileCreatesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	example := []byte("server:\n  addr: \":8080\"\n")

	created, err := EnsureFile(path, example)
	if err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}

	if !created {
		t.Error("created = false, want true for a path with no existing file")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(got) != string(example) {
		t.Errorf("written content = %q, want %q", got, example)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 0600 (may hold secrets once filled in)", perm)
	}
}

// TestEnsureFileNeverTouchesAnExistingFile is the important guard: this runs
// on every bot startup, including against a live production config.yaml that
// already exists and has been hand-edited. It must never overwrite it, no
// matter what the example content is.
func TestEnsureFileNeverTouchesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	original := []byte("server:\n  auth_token: \"do-not-lose-me\"\n")

	if err := os.WriteFile(path, original, 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	created, err := EnsureFile(path, []byte("this must never be written"))
	if err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}

	if created {
		t.Error("created = true, want false — the file already existed")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	if string(got) != string(original) {
		t.Errorf("existing file was modified: got %q, want the untouched original %q", got, original)
	}
}

func TestEnsureFileEmptyPathIsANoOp(t *testing.T) {
	created, err := EnsureFile("", []byte("irrelevant"))
	if err != nil {
		t.Fatalf("EnsureFile(\"\"): %v", err)
	}

	if created {
		t.Error("created = true for an empty path, want false — env-only deployments must be left alone")
	}
}

func TestEnsureFileCreatesAMissingParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "does", "not", "exist", "config.yaml")

	created, err := EnsureFile(path, []byte("ok"))
	if err != nil {
		t.Fatalf("EnsureFile: %v", err)
	}

	if !created {
		t.Error("created = false, want true")
	}

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was not created at the nested path: %v", err)
	}
}
