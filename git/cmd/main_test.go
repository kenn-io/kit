package gitcmd

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	code := m.Run()
	if err := os.RemoveAll(testGitDir); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
