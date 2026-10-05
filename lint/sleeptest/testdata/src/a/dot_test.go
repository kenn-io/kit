package a

import (
	"testing"
	. "time"
)

func TestDotImportedSleep(t *testing.T) {
	Sleep(Millisecond) // want "time.Sleep in a test outside a synctest bubble"
}
