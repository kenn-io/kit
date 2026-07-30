//go:build !windows

package packstore

import "os"

func replaceOwnershipFile(staged, target string) error {
	return os.Rename(staged, target)
}
