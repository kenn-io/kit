package secretref_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
	"hegel.dev/go/hegel"
)

func TestFromFieldsSources(t *testing.T) {
	for _, tc := range []struct {
		fields secretref.Fields
		want   secretref.Ref
	}{
		{secretref.Fields{}, secretref.Ref{}},
		{secretref.Fields{Value: "plain"}, secretref.Literal("plain")},
		{secretref.Fields{File: "~/token"}, secretref.Ref{File: "~/token"}},
		{secretref.Fields{Env: "APP_TOKEN"}, secretref.Ref{Env: "APP_TOKEN"}},
	} {
		got, err := secretref.FromFields("token", tc.fields)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
	_, err := secretref.FromFields("token", secretref.Fields{Value: "private-value", File: "~/token", Env: "APP_TOKEN"})
	require.ErrorContains(t, err, "token_file")
	assert.Contains(t, err.Error(), "token_env")
	assert.NotContains(t, err.Error(), "private-value")
}

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
	require.Error(t, err)
	t.Setenv(name+"_FILE", "")
	_, err = secretref.FromEnv(name)
	require.ErrorContains(t, err, name+"_FILE")
	require.NoError(t, os.Unsetenv(name))
	_, err = secretref.FromEnv(name)
	require.Error(t, err)
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

func TestSourceErrorReasonSafeAndNULRejected(t *testing.T) {
	t.Setenv("KIT_REASON_TOKEN", "")
	for _, ref := range []secretref.Ref{{Env: "KIT_REASON_TOKEN"}, secretref.Literal("secret\x00bytes")} {
		_, err := ref.Resolve()
		var sourceErr *secretref.SourceError
		require.ErrorAs(t, err, &sourceErr)
		assert.NotContains(t, sourceErr.Reason, "KIT_REASON_TOKEN")
		assert.NotContains(t, sourceErr.Reason, "secret\x00bytes")
	}
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

func TestJSONExactStringFields(t *testing.T) {
	for _, input := range []string{`{"value":null}`, `{"env":null}`, `{"file":null}`, `{"VALUE":"secret"}`, `{"Env":"APP_TOKEN"}`, `{"FILE":"/missing"}`} {
		var ref secretref.Ref
		require.Error(t, json.Unmarshal([]byte(input), &ref), input)
	}
}

func TestJSONWholeInput(t *testing.T) {
	for _, input := range []string{`{} {"value":"trailing"}`, `{}x`, `"key" null`, `null []`} {
		var ref secretref.Ref
		require.Error(t, ref.UnmarshalJSON([]byte(input)), input)
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

func TestLiteralSerializationScale(t *testing.T) {
	ref := secretref.Literal(strings.Repeat("a\n\"\\", 1<<18))
	encoded, err := json.Marshal(ref)
	require.NoError(t, err)
	var decoded secretref.Ref
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, ref, decoded)
	raw, err := ref.MarshalTOML()
	require.NoError(t, err)
	var doc document
	_, err = toml.Decode("key="+string(raw), &doc)
	require.NoError(t, err)
	assert.Equal(t, ref, doc.Key)
}

func TestFieldsRejectEveryConflictProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		mask := hegel.Draw(ht, hegel.Integers(0, 7))
		value := hegel.Draw(ht, hegel.Text())
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
			require.Error(ht, err)
			assert.NotContains(ht, err.Error(), "key:"+value)
		} else {
			require.NoError(ht, err)
			assert.Equal(ht, fields.Value, got.Value)
			assert.Equal(ht, fields.Env, got.Env)
			assert.Equal(ht, fields.File, got.File)
		}
	}, hegel.WithTestCases(2000))
}

func TestJSONArrayFieldRejected(t *testing.T) {
	var ref secretref.Ref
	require.Error(t, json.Unmarshal([]byte(`{"file":["secret"]}`), &ref))
}
