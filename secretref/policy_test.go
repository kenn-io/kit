package secretref_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
	"pgregory.net/rapid"
)

func TestSecretFormattingProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		secret := secretref.Secret{Value: rapid.String().Draw(ht, "text"), Source: "env:APP_TOKEN"}
		for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
			assert.Equal(ht, "env:APP_TOKEN: [redacted]", fmt.Sprintf(format, secret))
			assert.Equal(ht, "env:APP_TOKEN: [redacted]", fmt.Sprintf(format, &secret))
			other := secretref.Secret{Value: "different:" + secret.Value, Source: secret.Source}
			assert.Equal(ht, fmt.Sprintf(format, []secretref.Secret{secret}), fmt.Sprintf(format, []secretref.Secret{other}))
		}
		assert.Equal(ht, "env:APP_TOKEN: [redacted]", secret.String())
		assert.Equal(ht, secret.String(), secret.GoString())
		assert.Equal(ht, `"env:APP_TOKEN: [redacted]"`, fmt.Sprintf("%q", secret))
	})
}

func TestSecretJSONProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		secret := secretref.Secret{Value: string(rapid.SliceOf(rapid.Byte()).Draw(ht, "bytes")), Source: "inline"}
		data, err := json.Marshal(secret)
		require.NoError(ht, err)
		assert.JSONEq(ht, `{"source":"inline","value":"[redacted]"}`, string(data))
		pointer, err := json.Marshal(&secret)
		require.NoError(ht, err)
		assert.JSONEq(ht, string(data), string(pointer))
		nested, err := json.Marshal([]secretref.Secret{secret})
		require.NoError(ht, err)
		assert.JSONEq(ht, "["+string(data)+"]", string(nested))
	})
}

func TestSecretSlogProperty(t *testing.T) {
	rapid.Check(t, func(ht *rapid.T) {
		secret := secretref.Secret{Value: rapid.String().Draw(ht, "text"), Source: "inline"}
		var output bytes.Buffer
		slog.New(slog.NewJSONHandler(&output, nil)).Info("resolved", "secret", secret)
		var entry struct{ Secret map[string]string }
		require.NoError(ht, json.Unmarshal(output.Bytes(), &entry))
		assert.Equal(ht, map[string]string{"source": "inline", "value": "[redacted]"}, entry.Secret)
	})
}

func TestBlankLiteralDiagnosticSurvivesRedaction(t *testing.T) {
	secret, err := secretref.Literal(" ").Resolve()
	require.NoError(t, err)
	data, err := json.Marshal(secret)
	require.NoError(t, err)
	assert.JSONEq(t, `{"source":"inline","value":"[redacted]","reason":"inline value is empty"}`, string(data))
	var output bytes.Buffer
	slog.New(slog.NewJSONHandler(&output, nil)).Info("resolved", "secret", secret)
	assert.Contains(t, output.String(), `"reason":"inline value is empty"`)
}
