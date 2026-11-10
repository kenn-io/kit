package embedlocal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
)

// RecordChoice records an explicit turn-on of the built-in server. An error
// wrapping atomicfile.ErrPublished means the marker is visible; one wrapping
// atomicfile.ErrNotDurable means a crash may lose it.
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
func ChoiceRecorded() (bool, error) {
	dir, err := DefaultDir()
	if err != nil {
		return false, fmt.Errorf("resolve embeddings directory: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "search-by-meaning"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read search by meaning choice: %w", err)
	}
	return string(data) == "yes\n", nil
}
