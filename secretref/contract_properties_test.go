package secretref_test

import (
	"encoding/json"
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
		ref := secretref.Ref{Env: "OLD", File: "/old"}
		require.NoError(ht, ref.UnmarshalText([]byte(value)))
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
