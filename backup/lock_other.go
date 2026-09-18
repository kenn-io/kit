//go:build !windows

package backup

// lockFileBusy is always false: a rename does not fail because another handle
// has the file open.
func lockFileBusy(error) bool { return false }
