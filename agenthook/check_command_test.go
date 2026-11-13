package agenthook

import (
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

func TestPowerShellHookWords(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		want          []string
	}{
		{"generated literals", "& 'C:\\hook $%`\\hook.exe' 'example-marker' 'a''b $%`' ''", []string{"C:\\hook $%`\\hook.exe", "example-marker", "a'b $%`", ""}},
		{"short path", `C:\Users\RUNNER~1\hook.exe example-marker`, []string{`C:\Users\RUNNER~1\hook.exe`, "example-marker"}},
		{"percent", `C:\hook%1.exe 100%`, []string{`C:\hook%1.exe`, "100%"}},
		{"variable", `C:\hook.exe $value`, nil},
		{"double quote", `"C:\hook.exe" example-marker`, nil},
		{"compound", `C:\hook.exe example-marker & other`, nil},
		{"tilde", `~/hook example-marker`, nil},
		{"unterminated", `& 'C:\hook.exe`, nil},
		{"newline", "C:\\hook.exe\nother", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			words, err := powerShellHookWords(tc.command)
			if tc.want == nil {
				require.ErrorContains(t, err, "shell interpretation")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, words)
		})
	}
}
