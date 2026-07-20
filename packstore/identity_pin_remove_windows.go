//go:build windows

package packstore

import (
	"os"

	"golang.org/x/sys/windows"
)

func removeFileIdentityPinClaim(file *os.File, _ string) error {
	deleteFile := byte(1)
	return windows.SetFileInformationByHandle(
		windows.Handle(file.Fd()),
		windows.FileDispositionInfo,
		&deleteFile,
		1,
	)
}
