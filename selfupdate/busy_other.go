//go:build !windows

package selfupdate

func backupFileBusy(error) bool { return false }
