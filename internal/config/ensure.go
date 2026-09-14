package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureFile writes example to path if no file exists there yet, and reports
// whether it did. Load's own doc comment notes "a missing file is not an
// error" — true for reading, since env vars alone are enough to run — but it
// silently leaves nothing on disk, and the settings-save path (Patch) hard-
// requires the file to exist: the first live settings save against a
// never-initialized path fails with a confusing "read config: no such file"
// deep into a request instead of a clear message at startup. Called from
// cmd/bot before Load so a fresh deployment gets a real, editable file.
//
// Never touches an existing file: O_EXCL makes the existence check and the
// create atomic, so two processes racing to start against the same empty
// path cannot corrupt each other's write. An empty path is a deliberate
// env-only deployment (see Load) and is left alone.
func EnsureFile(path string, example []byte) (created bool, err error) {
	if path == "" {
		return false, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create config directory: %w", err)
	}

	// 0600: matches writeAtomic's own default for a freshly created config
	// file — it may come to hold broker secrets once filled in.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}

		return false, fmt.Errorf("create config %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Write(example); err != nil {
		return false, fmt.Errorf("write config %s: %w", path, err)
	}

	return true, nil
}
