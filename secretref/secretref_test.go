package secretref_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/safefileio"
	"go.kenn.io/kit/secretref"
)

type document struct {
	Key secretref.Ref `toml:"key" json:"key"`
}

func TestRefDecodesEveryFormFromTOMLAndJSON(t *testing.T) {
	tests := []struct {
		name string
		toml string
		json string
		want secretref.Ref
	}{
		{name: "literal", toml: `key = "sk-inline"`, json: `{"key":"sk-inline"}`, want: secretref.Ref{Value: "sk-inline"}},
		{name: "env", toml: `key = { env = "APP_KEY" }`, json: `{"key":{"env":"APP_KEY"}}`, want: secretref.Ref{Env: "APP_KEY"}},
		{name: "file", toml: `key = { file = "~/app.key" }`, json: `{"key":{"file":"~/app.key"}}`, want: secretref.Ref{File: "~/app.key"}},
		{name: "value table", toml: `key = { value = "sk-inline" }`, json: `{"key":{"value":"sk-inline"}}`, want: secretref.Ref{Value: "sk-inline"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var fromTOML document
			meta, err := toml.Decode(test.toml, &fromTOML)
			require.NoError(t, err)
			assert.Empty(t, meta.Undecoded())
			assert.Equal(t, test.want, fromTOML.Key)

			var fromJSON document
			require.NoError(t, json.Unmarshal([]byte(test.json), &fromJSON))
			assert.Equal(t, test.want, fromJSON.Key)

		})
	}
	for _, tc := range []struct{ input, want string }{
		{`{"VALUE":"t"}`, "t"},
		{`{"value":"t","env":null}`, "t"},
		{`"t\u0000"`, "t\x00"},
	} {
		var ref secretref.Ref
		require.NoError(t, json.Unmarshal([]byte(tc.input), &ref))
		assert.Equal(t, secretref.Ref{Value: tc.want}, ref)
	}
	for _, input := range []string{`null`, `""`, `{}`, `{"value":""}`} {
		var ref secretref.Ref
		require.NoError(t, json.Unmarshal([]byte(input), &ref))
		assert.True(t, ref.IsZero())
	}
	for _, input := range []string{`key = ""`, `key = {value=""}`} {
		var doc document
		_, err := toml.Decode(input, &doc)
		require.NoError(t, err)
		assert.True(t, doc.Key.IsZero())
	}
	var ref secretref.Ref
	require.NoError(t, json.Unmarshal([]byte(`{"value":"first","value":"last"}`), &ref))
	assert.Equal(t, secretref.Ref{Value: "last"}, ref)
	require.NoError(t, json.Unmarshal([]byte(`{"value":"first","value":""}`), &ref))
	assert.True(t, ref.IsZero())
}

func TestRefRejectsAmbiguousOrUnknownSources(t *testing.T) {
	for _, input := range []string{
		`key = { env = "A", file = "~/a.key" }`,
		`key = { value = "sk", env = "A" }`,
		`key = { vault = "app/key" }`,
		`key = { env = 1 }`,
		`key = 1`,
	} {
		var decoded document
		_, err := toml.Decode(input, &decoded)
		require.Error(t, err, input)
	}
	var decoded document
	require.Error(t, json.Unmarshal([]byte(`{"key":{"env":"A","file":"~/a.key"}}`), &decoded))
	require.Error(t, json.Unmarshal([]byte(`{"key":{"vault":"app/key"}}`), &decoded))
	require.Error(t, json.Unmarshal([]byte(`{"key":{"file":["secret"]}}`), &decoded))
	for _, ref := range []secretref.Ref{
		{Value: "sk", Env: "A"},
		{Value: "sk", File: "~/a.key"},
		{Env: "A", File: "~/a.key"},
	} {
		require.Error(t, ref.Validate())
		_, err := ref.MarshalTOML()
		require.Error(t, err)
	}
}

func TestRefResolve(t *testing.T) {
	t.Setenv("KIT_TEST_SECRET", "from-env")
	t.Setenv("KIT_TEST_EMPTY", "")
	dir := privateTempDir(t)
	keyFile := filepath.Join(dir, "app.key")
	writePrivateFile(t, keyFile, "from-file\r\n")
	absent := filepath.Join(dir, "absent.key")
	t.Chdir(dir)
	var relative document
	_, err := toml.Decode(`key = {file = "app.key"}`, &relative)
	require.NoError(t, err)

	tests := []struct {
		name    string
		ref     secretref.Ref
		want    secretref.Secret
		wantErr string
	}{
		{name: "unset", ref: secretref.Ref{}, want: secretref.Secret{}},
		{name: "literal NUL", ref: secretref.Literal("t\x00"), want: secretref.Secret{Value: "t\x00", Source: "inline"}},
		{name: "relative file", ref: relative.Key, want: secretref.Secret{Value: "from-file", Source: "file:app.key"}},
		{name: "padded file", ref: secretref.Ref{File: " " + keyFile + " "}, want: secretref.Secret{Value: "from-file", Source: "file:" + keyFile}},
		{name: "literal", ref: secretref.Literal("sk-inline"), want: secretref.Secret{Value: "sk-inline", Source: "inline"}},
		{name: "literal is not expanded", ref: secretref.Literal("${KIT_TEST_SECRET}"), want: secretref.Secret{
			Value: "${KIT_TEST_SECRET}", Source: "inline",
		}},
		{name: "blank literal", ref: secretref.Literal(" "), want: secretref.Secret{Source: "inline", Reason: "inline value is empty"}},
		{name: "env", ref: secretref.Ref{Env: "KIT_TEST_SECRET"}, want: secretref.Secret{Value: "from-env", Source: "env:KIT_TEST_SECRET"}},
		{name: "empty env", ref: secretref.Ref{Env: "KIT_TEST_EMPTY"}, wantErr: "secretref: env:KIT_TEST_EMPTY: secret is unset or empty"},
		{name: "blank file selector", ref: secretref.Ref{File: "  "}, wantErr: "file selector is empty"},
		{name: "file", ref: secretref.Ref{File: keyFile}, want: secretref.Secret{Value: "from-file", Source: "file:" + keyFile}},
		{name: "missing file", ref: secretref.Ref{File: absent}, wantErr: "file is missing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.ref.Resolve()
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.want, got)
		})
	}
}

func TestRefFileExpandsHome(t *testing.T) {
	home := privateTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	writePrivateFile(t, filepath.Join(home, "app.key"), "from-home")

	got, err := secretref.Ref{File: "~/app.key"}.Resolve()
	require.NoError(t, err)
	assert.Equal(t, secretref.Secret{Value: "from-home", Source: "file:~/app.key"}, got)
}

func writePrivateFile(t *testing.T, path, contents string) {
	t.Helper()
	file, err := safefileio.CreatePrivateFile(path)
	require.NoError(t, err)
	_, err = file.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}
