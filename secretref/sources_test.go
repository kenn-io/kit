package secretref_test

import (
	"os"
	"strings"
	"testing"

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
	require.ErrorContains(t, err, "file selector is empty")
	t.Setenv(name+"_FILE", "/private/token ")
	_, err = secretref.FromEnv(name)
	require.ErrorContains(t, err, "surrounding whitespace")
	t.Setenv(name+"_FILE", "/nonexistent/example.key")
	got, err = secretref.FromEnv(name)
	require.NoError(t, err)
	assert.Equal(t, secretref.Ref{File: "/nonexistent/example.key"}, got)
	for _, name := range []string{"", " ", " APP_TOKEN", "APP_TOKEN ", "A=B", "A\x00B"} {
		_, err := secretref.FromEnv(name)
		require.Error(t, err)
	}
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
