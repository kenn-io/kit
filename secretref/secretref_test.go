package secretref_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/secretref"
)

func TestRefResolve(t *testing.T) {
	t.Setenv("KIT_TEST_SECRET", "from-env")
	t.Setenv("KIT_TEST_EMPTY", "")
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "app.key")
	writePrivateFile(t, keyFile, "from-file\r\n")
	emptyFile := filepath.Join(dir, "empty.key")
	writePrivateFile(t, emptyFile, "\n")
	absent := filepath.Join(dir, "absent.key")

	tests := []struct {
		ref  secretref.Ref
		want secretref.Secret
	}{
		{ref: "", want: secretref.Secret{}},
		{ref: "sk-inline", want: secretref.Secret{Value: "sk-inline", Source: "inline"}},
		{ref: " ", want: secretref.Secret{Source: "inline", Reason: "inline value is empty"}},
		{ref: "env:KIT_TEST_SECRET", want: secretref.Secret{Value: "from-env", Source: "env:KIT_TEST_SECRET"}},
		{ref: "env:KIT_TEST_EMPTY", want: secretref.Secret{
			Source: "env:KIT_TEST_EMPTY", Reason: "env KIT_TEST_EMPTY is unset or empty",
		}},
		{ref: secretref.Ref("file:" + keyFile), want: secretref.Secret{Value: "from-file", Source: "file:" + keyFile}},
		{ref: secretref.Ref("file:" + absent), want: secretref.Secret{Source: "file:" + absent, Reason: "file is missing"}},
		{ref: secretref.Ref("file:" + emptyFile), want: secretref.Secret{Source: "file:" + emptyFile, Reason: "file is empty"}},
	}
	for _, test := range tests {
		t.Run(string(test.ref), func(t *testing.T) {
			got, err := test.ref.Resolve()
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestRefFileExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writePrivateFile(t, filepath.Join(home, "app.key"), "from-home")

	got, err := secretref.Ref("file:~/app.key").Resolve()
	require.NoError(t, err)
	assert.Equal(t, secretref.Secret{Value: "from-home", Source: "file:~/app.key"}, got)
}

func TestRefRejectsMalformedReferences(t *testing.T) {
	for _, ref := range []secretref.Ref{"vault:app/key", "https://example.test", "env:", "file: "} {
		t.Run(string(ref), func(t *testing.T) {
			require.Error(t, ref.Validate())
			_, err := ref.Resolve()
			require.Error(t, err)
		})
	}
	// A value whose prefix is not a lowercase word is an inline secret.
	for _, ref := range []secretref.Ref{"Bearer:abc", "sk-proj:abc", "pa-9f2c"} {
		require.NoError(t, ref.Validate(), string(ref))
	}
}

func writePrivateFile(t *testing.T, path, contents string) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
