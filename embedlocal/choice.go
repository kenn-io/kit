package embedlocal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// RecordChoice records an explicit turn-on of the built-in server. Apps can
// also call it at startup for an existing built-in server opt-in.
func RecordChoice() error {
	dir, err := DefaultDir()
	if err != nil {
		return fmt.Errorf("resolve embeddings directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare embeddings directory: %w", err)
	}
	if err := atomicfile.WriteFile(filepath.Join(dir, "search-by-meaning"), []byte("yes\n")); err != nil {
		return fmt.Errorf("record search by meaning choice: %w", err)
	}
	return nil
}

// ChoiceRecorded reports whether a user recorded a shared turn-on choice.
// On error, callers should leave the shared choice off.
func ChoiceRecorded() (bool, error) {
	dir, err := DefaultDir()
	if err != nil {
		return false, fmt.Errorf("resolve embeddings directory: %w", err)
	}
	file, err := os.Open(filepath.Join(dir, "search-by-meaning"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open search by meaning choice: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 5))
	if err != nil {
		return false, fmt.Errorf("read search by meaning choice: %w", err)
	}
	return string(data) == "yes\n", nil
}
