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
)

func TestSecretFormatting(t *testing.T) {
	secret := secretref.Secret{Value: "sample-token", Source: "env:APP_TOKEN"}
	for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
		assert.Equal(t, "env:APP_TOKEN: [redacted]", fmt.Sprintf(format, secret))
		assert.Equal(t, "env:APP_TOKEN: [redacted]", fmt.Sprintf(format, &secret))
		other := secretref.Secret{Value: "different:" + secret.Value, Source: secret.Source}
		assert.Equal(t, fmt.Sprintf(format, []secretref.Secret{secret}), fmt.Sprintf(format, []secretref.Secret{other}))
	}
	assert.Equal(t, `"env:APP_TOKEN: [redacted]"`, fmt.Sprintf("%q", secret))
	for _, format := range []string{"%d", "%f", "%x"} {
		assert.NotContains(t, fmt.Sprintf(format, secret), secret.Value)
		assert.NotContains(t, fmt.Sprintf(format, &secret), secret.Value)
	}
}

func TestSecretJSON(t *testing.T) {
	for _, tc := range []struct {
		secret secretref.Secret
		want   string
	}{
		{secretref.Secret{Value: "sample-token", Source: "inline"}, `{"source":"inline","value":"[redacted]"}`},
		{secretref.Secret{Source: "inline", Reason: "inline value is empty"}, `{"source":"inline","value":"[redacted]","reason":"inline value is empty"}`},
	} {
		data, err := json.Marshal(tc.secret)
		require.NoError(t, err)
		assert.JSONEq(t, tc.want, string(data))
		pointer, err := json.Marshal(&tc.secret)
		require.NoError(t, err)
		assert.JSONEq(t, string(data), string(pointer))
		nested, err := json.Marshal([]secretref.Secret{tc.secret})
		require.NoError(t, err)
		assert.JSONEq(t, "["+string(data)+"]", string(nested))
	}
}

func TestSecretSlog(t *testing.T) {
	for _, secret := range []secretref.Secret{
		{Value: "sample-token", Source: "inline"},
		{Source: "inline", Reason: "inline value is empty"},
	} {
		var output bytes.Buffer
		slog.New(slog.NewTextHandler(&output, nil)).Info("resolved", "secret", secret)
		assert.Contains(t, output.String(), "secret.source=inline")
		assert.Contains(t, output.String(), "secret.value=[redacted]")
		assert.NotContains(t, output.String(), "sample-token")
		if secret.Reason != "" {
			assert.Contains(t, output.String(), `secret.reason="inline value is empty"`)
		}
	}
}
