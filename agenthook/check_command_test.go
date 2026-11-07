package agenthook

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPosixHookExecutable(t *testing.T) {
	for _, tc := range []struct{ name, command, want string }{
		{"quoted executable with arguments", `'/example (stable)/hook' example-marker \* [literal] "$EXAMPLE_HOOK"`, "/example (stable)/hook"},
		{"escaped executable", `/example/hook\[stable\] example-marker`, "/example/hook[stable]"},
		{"shell assignment", "EXAMPLE_HOOK=1 example-hook example-marker", ""},
		{"shell expansion", "${EXAMPLE_HOOK:-existing-program} example-marker", ""},
		{"quoted expansion", `"$EXAMPLE_HOOK" example-marker`, ""},
		{"glob executable", "*hook example-marker", ""},
		{"tilde executable", "~/hook example-marker", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := posixHookExecutable(tc.command)
			if tc.want == "" {
				require.ErrorContains(t, err, "shell interpretation")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRawHookExecutableWindowsCompound(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("CommandWindows is selected on Windows")
	}
	_, err := rawHookExecutable("example-missing-hook-executable example-marker", `"C:\example (stable).exe" example-marker & example-missing-hook-executable`)
	require.ErrorContains(t, err, "shell interpretation")
}
