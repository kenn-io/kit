package selfupdate

import (
	"errors"

	"golang.org/x/sys/windows"
)

func backupFileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
