//go:build !windows

package renamebusy

func fileBusy(error) bool { return false }
