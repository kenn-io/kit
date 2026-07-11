package packstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
)

// mkdirAllImportSynced creates independent directories below root and syncs
// each parent entry as it becomes visible. name has already passed the import
// content-directory validation and is always slash-separated.
func mkdirAllImportSynced(root *os.Root, name string) error {
	current := "."
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." {
			continue
		}
		parent := current
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("packstore: import directory %s is not an independent directory", current)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := root.Mkdir(current, 0o700); err != nil {
			return err
		}
		if err := syncImportRootDir(root, parent); err != nil {
			return err
		}
	}
	return nil
}
