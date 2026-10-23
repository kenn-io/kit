package renamebusy

import (
	"errors"

	"golang.org/x/sys/windows"
)

func fileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
