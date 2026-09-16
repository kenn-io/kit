package gclplugin

import (
	"testing"

	"github.com/golangci/plugin-module-register/register"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/lint/errtext"
)

func analyzerNames(t *testing.T, p register.LinterPlugin) []string {
	t.Helper()
	analyzers, err := p.BuildAnalyzers()
	require.NoError(t, err)
	names := make([]string, 0, len(analyzers))
	for _, a := range analyzers {
		names = append(names, a.Name)
	}
	return names
}

func TestPluginIsRegistered(t *testing.T) {
	assert := assert.New(t)
	newPlugin, err := register.GetPlugin(Name)
	require.NoError(t, err)
	p, err := newPlugin(nil)
	require.NoError(t, err)
	assert.Equal([]string{"errtext", "nohttpmux", "sleeptest", "sqlenum", "testifyhelper"}, analyzerNames(t, p))
	assert.Equal(register.LoadModeTypesInfo, p.GetLoadMode())
	assert.False(errtext.IncludeTests)
}

func TestPluginSettings(t *testing.T) {
	assert := assert.New(t)
	t.Cleanup(func() { errtext.IncludeTests = false })
	p, err := New(map[string]any{
		"disable": []any{"sleeptest", "nohttpmux"},
		"errtext": map[string]any{"include-tests": true},
	})
	require.NoError(t, err)
	assert.Equal([]string{"errtext", "sqlenum", "testifyhelper"}, analyzerNames(t, p))
	assert.True(errtext.IncludeTests)
}

func TestPluginRejectsUnknownSettings(t *testing.T) {
	assert := assert.New(t)
	_, err := New(map[string]any{"disable": []any{"nope"}})
	require.ErrorContains(t, err, `unknown analyzer "nope"`)

	_, err = New(map[string]any{"bogus": true})
	assert.ErrorContains(err, "kennlint settings")
}
