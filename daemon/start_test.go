package daemon_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/kit/daemon"
)

func TestIsEphemeralExecutableDetectsTestBinaries(t *testing.T) {
	assert.True(t, daemon.IsEphemeralExecutable("/tmp/tool.test"))
	assert.True(t, daemon.IsEphemeralExecutable(`C:\Temp\tool.test.exe`))
	assert.True(t, daemon.IsEphemeralExecutable(`C:\Temp\tool.TEST.EXE`))
	assert.False(t, daemon.IsEphemeralExecutable("/usr/local/bin/tool"))
}

func TestIsEphemeralExecutableDetectsGoBuildPaths(t *testing.T) {
	assert.True(t, daemon.IsEphemeralExecutable("/tmp/go-build123/tool"))
	assert.True(t, daemon.IsEphemeralExecutable(`C:\Users\me\AppData\Local\Temp\go-build123\tool.exe`))
}
