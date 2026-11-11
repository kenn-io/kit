package secretref_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
)

func TestFileBytesAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, value, want string
		fail              bool
		wantReason        string
	}{
		{"preserve spaces", "  token \t\r\n\n", "  token \t", false, ""},
		{"one byte", "s", "s", false, ""},
		{"below limit", strings.Repeat("s", (64<<10)-1), strings.Repeat("s", (64<<10)-1), false, ""},
		{"limit", strings.Repeat("s", 64<<10), strings.Repeat("s", 64<<10), false, ""},
		{"too large", strings.Repeat("s", (64<<10)+1), "", true, "file exceeds 64 KiB"},
		{"trim cannot hide size", "s" + strings.Repeat("\n", 64<<10), "", true, "file exceeds 64 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "token")
			writePrivateFile(t, path, tc.value)
			got, err := (secretref.Ref{File: path}).ResolveContext(t.Context())
			if tc.fail {
				require.Error(t, err)
				var source *secretref.SourceError
				require.ErrorAs(t, err, &source)
				assert.NotContains(t, source.Reason, path)
				if tc.wantReason != "" {
					assert.Equal(t, tc.wantReason, source.Reason)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got.Value)
			}
		})
	}
}
