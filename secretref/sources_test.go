package secretref_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
)

func TestFromEnvPresenceAndNoIO(t *testing.T) {
	const name = "KIT_SOURCE_TEST"
	t.Setenv(name, "")
	t.Setenv(name+"_FILE", "")
	require.NoError(t, os.Unsetenv(name))
	require.NoError(t, os.Unsetenv(name+"_FILE"))
	got, err := secretref.FromEnv(name)
	require.NoError(t, err)
	assert.True(t, got.IsZero())
	t.Setenv(name, "")
	got, err = secretref.FromEnv(name)
	require.NoError(t, err)
	assert.Equal(t, secretref.Ref{Env: name}, got)
	t.Setenv(name+"_FILE", "")
	_, err = secretref.FromEnv(name)
	require.ErrorContains(t, err, name+"_FILE")
	require.NoError(t, os.Unsetenv(name))
	_, err = secretref.FromEnv(name)
	require.Error(t, err)
	t.Setenv(name+"_FILE", "/private/token ")
	_, err = secretref.FromEnv(name)
	require.ErrorContains(t, err, "surrounding whitespace")
	t.Setenv(name+"_FILE", "/nonexistent/example.key")
	got, err = secretref.FromEnv(name)
	require.NoError(t, err)
	assert.Equal(t, secretref.Ref{File: "/nonexistent/example.key"}, got)
}

func TestFromEnvRejectsAmbiguousNames(t *testing.T) {
	for _, name := range []string{"", " ", " APP_TOKEN", "APP_TOKEN ", "A=B", "A\x00B"} {
		_, err := secretref.FromEnv(name)
		require.Error(t, err)
	}
}

func TestExactEmptyCompatibility(t *testing.T) {
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
}

func TestJSONDuplicateStringKeysKeepLastValue(t *testing.T) {
	// Ref preserves the released decoder behavior for duplicate JSON members.
	var ref secretref.Ref
	require.NoError(t, json.Unmarshal([]byte(`{"value":"first","value":"last"}`), &ref))
	assert.Equal(t, secretref.Literal("last"), ref)
	require.NoError(t, json.Unmarshal([]byte(`{"value":"first","value":""}`), &ref))
	assert.True(t, ref.IsZero())
}

func TestFromFields(t *testing.T) {
	for _, tc := range []struct {
		fields secretref.Fields
		want   secretref.Ref
	}{
		{secretref.Fields{}, secretref.Ref{}},
		{secretref.Fields{Value: "secret"}, secretref.Literal("secret")},
		{secretref.Fields{Env: "TOKEN"}, secretref.Ref{Env: "TOKEN"}},
		{secretref.Fields{File: "token"}, secretref.Ref{File: "token"}},
	} {
		got, err := secretref.FromFields("token", tc.fields)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	_, err := secretref.FromFields("token", secretref.Fields{Value: "private-value", Env: "TOKEN"})
	require.ErrorContains(t, err, "token")
	assert.Equal(t, 1, strings.Count(err.Error(), "secretref:"))
	assert.NotContains(t, err.Error(), "private-value")
}
