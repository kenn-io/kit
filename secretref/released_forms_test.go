package secretref_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
)

func TestReleasedFormsResolve(t *testing.T) {
	dir := privateTempDir(t)
	require.NoError(t, os.Chmod(dir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "keys"), 0o700))
	path := filepath.Join(dir, "keys", "token")
	require.NoError(t, os.WriteFile(path, []byte("t"), 0o600))
	t.Chdir(dir)
	padded, err := json.Marshal(map[string]string{"file": " " + path + " "})
	require.NoError(t, err)
	for _, input := range []string{`{"VALUE":"t"}`, `{"value":"t","env":null}`, string(padded), `"t\u0000"`} {
		t.Run(input, func(t *testing.T) {
			var ref secretref.Ref
			require.NoError(t, json.Unmarshal([]byte(input), &ref))
			secret, err := ref.Resolve()
			require.NoError(t, err)
			want := "t"
			if input == `"t\u0000"` {
				want = "t\x00"
			}
			require.Equal(t, want, secret.Value)
		})
	}
	var doc document
	_, err = toml.Decode(`key = {file = "keys/token"}`, &doc)
	require.NoError(t, err)
	fileSecret, err := doc.Key.Resolve()
	require.NoError(t, err)
	require.Equal(t, "t", fileSecret.Value)
	secret, err := secretref.Literal(" ").Resolve()
	require.NoError(t, err)
	require.Equal(t, "inline value is empty", secret.Reason)
}
