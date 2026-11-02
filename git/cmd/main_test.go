package gitcmd

import (
	"os"
	"testing"
)

var testGitDir string

func TestMain(m *testing.M) {
	code := m.Run()
	if testGitDir != "" {
		if err := os.RemoveAll(testGitDir); err != nil {
			panic(err)
		}
	}
	os.Exit(code)
}
