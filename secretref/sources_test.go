package secretref_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
	"pgregory.net/rapid"
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
	_, err = got.Resolve()
	require.EqualError(t, err, "secretref: env:KIT_SOURCE_TEST: secret is unset or empty")
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

func TestResolveContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := secretref.Literal("key").ResolveContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestExactEmptyCompatibility(t *testing.T) {
	for _, input := range []string{`null`, `""`, `{}`, `{"value":""}`} {
		var ref secretref.Ref
		require.NoError(t, json.Unmarshal([]byte(input), &ref))
		assert.True(t, ref.IsZero())
		got, err := ref.Resolve()
		require.NoError(t, err)
		assert.Equal(t, secretref.Secret{}, got)
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

func TestFieldsRejectEveryConflictProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		mask := rapid.IntRange(0, 7).Draw(ht, "mask")
		value := rapid.String().Draw(ht, "text")
		fields := secretref.Fields{}
		count := 0
		if mask&1 != 0 {
			fields.Value = "key:" + value
			count++
		}
		if mask&2 != 0 {
			fields.Env = "APP_TOKEN"
			count++
		}
		if mask&4 != 0 {
			fields.File = "~/token"
			count++
		}
		got, err := secretref.FromFields("token", fields)
		if count > 1 {
			require.ErrorContains(ht, err, "token")
			assert.NotContains(ht, err.Error(), "key:"+value)
		} else {
			require.NoError(ht, err)
			assert.Equal(ht, fields.Value, got.Value)
			assert.Equal(ht, fields.Env, got.Env)
			assert.Equal(ht, fields.File, got.File)
		}
	})
}
