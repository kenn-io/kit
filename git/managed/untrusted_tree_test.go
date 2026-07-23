package managedworktree

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSupportsUntrustedTreeCheckoutGitVersion(t *testing.T) {
	for _, test := range []struct {
		output string
		goos   string
		want   bool
	}{
		{output: "git version 2.39.0", goos: "linux"},
		{output: "git version 2.39.1", goos: "linux", want: true},
		{output: "git version 2.45.2 (Apple Git-145)", goos: "darwin", want: true},
		{output: "git version 2.52.2.windows.4", goos: "linux"},
		{output: "git version 2.52.2.windows.4", goos: "windows"},
		{output: "git version 2.53.0", goos: "windows"},
		{output: "git version 2.53.0.windows.2", goos: "windows"},
		{output: "git version 2.53.0.windows.3-malformed", goos: "windows"},
		{output: "git version 2.53.0.windows.3", goos: "linux", want: true},
		{output: "git version 2.53.0.windows.3", goos: "windows", want: true},
		{output: "git version 2.53.1.windows.1", goos: "windows", want: true},
		{output: "git version 2.54.0.windows.1", goos: "windows", want: true},
		{output: "not git", goos: "linux"},
	} {
		assert.Equal(t, test.want,
			supportsUntrustedTreeCheckoutGitVersion(test.output, test.goos),
			test.output)
	}
}
