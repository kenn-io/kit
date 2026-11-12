package agenthook

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPosixHookWords(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		want          []string
	}{
		{"quoted executable with arguments", `'/example (stable)/hook' example-marker \* [literal] "$EXAMPLE_HOOK"`, []string{"/example (stable)/hook", "example-marker", "*", "\x00[literal]", "\x00\"$EXAMPLE_HOOK\""}},
		{"escaped executable", `/example/hook\[stable\] example-marker`, []string{"/example/hook[stable]", "example-marker"}},
		{"equivalent quoting", `/example/hook "example-marker"`, []string{"/example/hook", "example-marker"}},
		{"single quoted home argument", `/example/hook example-marker '$HOME'`, []string{"/example/hook", "example-marker", "$HOME"}},
		{"shell assignment", "EXAMPLE_HOOK=1 example-hook example-marker", nil},
		{"shell expansion", "${EXAMPLE_HOOK:-existing-program} example-marker", nil},
		{"quoted expansion", `"$EXAMPLE_HOOK" example-marker`, nil},
		{"glob executable", "*hook example-marker", nil},
		{"tilde executable", "~/hook example-marker", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := posixHookWords(tc.command)
			if tc.want == nil {
				require.ErrorContains(t, err, "shell interpretation")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRawHookWordsWindowsCompound(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("CommandWindows is selected on Windows")
	}
	_, err := rawHookWords("example-missing-hook-executable example-marker", `"C:\example (stable).exe" example-marker & example-missing-hook-executable`)
	require.ErrorContains(t, err, "shell interpretation")
	words, err := rawHookWords("", `C:\Users\RUNNER~1\hook.exe example-marker`)
	require.NoError(t, err)
	assert.Equal(t, []string{`C:\Users\RUNNER~1\hook.exe`, "example-marker"}, words)
}
