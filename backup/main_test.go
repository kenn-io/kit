package backup

import (
	"os"
	"testing"
)

var largeFixtureDir string

func TestMain(m *testing.M) {
	os.Exit(runBackupTests(m))
}

func runBackupTests(m *testing.M) int {
	var err error
	largeFixtureDir, err = os.MkdirTemp("", "kit-backup-fixtures-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(largeFixtureDir)
	return m.Run()
}
