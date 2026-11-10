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
	"hegel.dev/go/hegel"
)

func TestRelativeFileRejectedAtLoad(t *testing.T) {
	require.Error(t, (secretref.Ref{File: "keys/token"}).Validate())
	var ref secretref.Ref
	require.Error(t, json.Unmarshal([]byte(`{"file":"keys/token"}`), &ref))
}

func TestSecretFormattingProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		secret := secretref.Secret{Value: hegel.Draw(ht, hegel.Text()), Source: "env:APP_TOKEN"}
		for _, format := range []string{"%s", "%v", "%+v", "%#v"} {
			assert.Equal(ht, "env:APP_TOKEN: [redacted]", fmt.Sprintf(format, secret))
			assert.Equal(ht, "env:APP_TOKEN: [redacted]", fmt.Sprintf(format, &secret))
			other := secretref.Secret{Value: "different:" + secret.Value, Source: secret.Source}
			assert.Equal(ht, fmt.Sprintf(format, []secretref.Secret{secret}), fmt.Sprintf(format, []secretref.Secret{other}))
		}
		assert.Equal(ht, "env:APP_TOKEN: [redacted]", secret.String())
		assert.Equal(ht, secret.String(), secret.GoString())
		assert.Equal(ht, `"env:APP_TOKEN: [redacted]"`, fmt.Sprintf("%q", secret))
	}, hegel.WithTestCases(2000))
}

func TestSecretJSONProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		secret := secretref.Secret{Value: string(hegel.Draw(ht, hegel.Binary(0, -1))), Source: "inline"}
		data, err := json.Marshal(secret)
		require.NoError(ht, err)
		assert.JSONEq(ht, `{"source":"inline","value":"[redacted]"}`, string(data))
		direct, err := secret.MarshalJSON()
		require.NoError(ht, err)
		assert.JSONEq(ht, string(data), string(direct))
		pointer, err := json.Marshal(&secret)
		require.NoError(ht, err)
		assert.JSONEq(ht, string(data), string(pointer))
		nested, err := json.Marshal([]secretref.Secret{secret})
		require.NoError(ht, err)
		assert.JSONEq(ht, "["+string(data)+"]", string(nested))
	}, hegel.WithTestCases(2000))
}

func TestSecretSlogProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		secret := secretref.Secret{Value: hegel.Draw(ht, hegel.Text()), Source: "inline"}
		var output bytes.Buffer
		slog.New(slog.NewJSONHandler(&output, nil)).Info("resolved", "secret", secret)
		var entry struct{ Secret map[string]string }
		require.NoError(ht, json.Unmarshal(output.Bytes(), &entry))
		assert.Equal(ht, map[string]string{"source": "inline", "value": "[redacted]"}, entry.Secret)
	}, hegel.WithTestCases(2000))
}

func TestBlankInlineFails(t *testing.T) {
	_, err := secretref.Literal(" \t").Resolve()
	require.Error(t, err)
}

func TestExtendedSecretDiagnostics(t *testing.T) {
	secret := secretref.Secret{Value: "do-not-log", Source: "env:APP_TOKEN"}
	value := secret.LogValue()
	assert.Equal(t, slog.KindGroup, value.Kind())
	assert.Equal(t, []slog.Attr{slog.String("source", secret.Source), slog.String("value", "[redacted]")}, value.Group())
	var output bytes.Buffer
	slog.New(slog.NewTextHandler(&output, nil)).WithGroup("provider").Info("resolved", "secret", &secret)
	assert.Contains(t, output.String(), "APP_TOKEN")
	assert.Contains(t, output.String(), "[redacted]")
	assert.NotContains(t, output.String(), secret.Value)
	err := &secretref.SourceError{Source: secret.Source, Reason: "secret is unset or empty"}
	assert.Equal(t, "secretref: env:APP_TOKEN: secret is unset or empty", err.Error())
}
