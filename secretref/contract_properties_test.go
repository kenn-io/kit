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
	"pgregory.net/rapid"
)

func TestLiteralAndTextKeepAllBytesProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		value := string(rapid.SliceOf(rapid.Byte()).Draw(ht, "bytes"))
		ref := secretref.Literal(value)
		assert.Equal(ht, secretref.Ref{Value: value}, ref)
		ref = secretref.Ref{Env: "OLD", File: "/old"}
		require.NoError(ht, ref.UnmarshalText([]byte(value)))
		assert.Equal(ht, secretref.Literal(value), ref)
	})
}

func TestTOMLHookLiteralProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		value := rapid.String().Draw(ht, "text")
		for _, input := range []any{value, map[string]any{"value": value}} {
			ref := secretref.Ref{Env: "OLD"}
			require.NoError(ht, ref.UnmarshalTOML(input))
			assert.Equal(ht, secretref.Literal(value), ref)
		}
	})
}

func TestJSONWholeObjectProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		value := rapid.String().Draw(ht, "text")
		data, err := json.Marshal(map[string]string{"value": value})
		require.NoError(ht, err)
		var ref secretref.Ref
		require.NoError(ht, ref.UnmarshalJSON(append(data, ' ', '\n', '\t')))
		assert.Equal(ht, secretref.Literal(value), ref)
	})
}

func TestAllRefSourceRoundtripsProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		text := rapid.String().Draw(ht, "text")
		ref := secretref.Literal(text)
		switch rapid.IntRange(0, 2).Draw(ht, "choice") {
		case 1:
			ref = secretref.Ref{Env: "ENV_" + strings.NewReplacer("=", "_", "\x00", "_").Replace(text)}
		case 2:
			ref = secretref.Ref{File: "~/" + strings.ReplaceAll(text, "\x00", "_")}
		}
		data, err := json.Marshal(ref)
		require.NoError(ht, err)
		var decoded secretref.Ref
		require.NoError(ht, json.Unmarshal(data, &decoded))
		assert.Equal(ht, ref, decoded)
		raw, err := ref.MarshalTOML()
		require.NoError(ht, err)
		var doc document
		_, err = toml.Decode("key="+string(raw), &doc)
		require.NoError(ht, err)
		assert.Equal(ht, ref, doc.Key)
		type fieldsOnly secretref.Ref
		data, err = toml.Marshal(struct{ Key fieldsOnly }{fieldsOnly(ref)})
		require.NoError(ht, err)
		var fieldDoc document
		meta, err := toml.Decode(string(data), &fieldDoc)
		require.NoError(ht, err)
		assert.Empty(ht, meta.Undecoded())
		assert.Equal(ht, ref, fieldDoc.Key)
	})
}

func TestLiteralResolveAgreementProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		value := string(rapid.SliceOf(rapid.Byte()).Draw(ht, "bytes"))
		ref := secretref.Literal(value)
		got, err := ref.ResolveContext(t.Context())
		wrapper, wrapperErr := ref.Resolve()
		assert.Equal(ht, got, wrapper)
		assert.Equal(ht, err == nil, wrapperErr == nil)
		switch {
		case value == "":
			require.NoError(ht, err)
			assert.Equal(ht, secretref.Secret{}, got)
		case strings.TrimSpace(value) == "":
			require.NoError(ht, err)
			assert.Equal(ht, secretref.Secret{Source: "inline", Reason: "inline value is empty"}, got)
		default:
			require.NoError(ht, err)
			assert.Equal(ht, secretref.Secret{Value: value, Source: "inline"}, got)
		}
	})
}

func TestEnvSelectionReadsLatestValueProperty(t *testing.T) {
	const name = "KIT_LATEST_SECRET_TEST"
	t.Setenv(name, "selected")
	t.Setenv(name+"_FILE", "")
	require.NoError(t, os.Unsetenv(name+"_FILE"))
	ref, err := secretref.FromEnv(name)
	require.NoError(t, err)
	rapid.Check(t, func(ht *rapid.T) {
		// Construct an OS-representable value; literal properties cover NUL.
		value := strings.ReplaceAll(rapid.String().Draw(ht, "text"), "\x00", "_")
		t.Setenv(name, value)
		got, err := ref.ResolveContext(t.Context())
		if strings.TrimSpace(value) == "" {
			require.Error(ht, err)
		} else {
			require.NoError(ht, err)
			assert.Equal(ht, secretref.Secret{Value: value, Source: "env:" + name}, got)
		}
	})
}
