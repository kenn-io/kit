package gclplugin

import (
	"testing"

	"github.com/golangci/plugin-module-register/register"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/lint/errtext"
	"go.kenn.io/kit/lint/sleeptest"
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
	assert.Equal([]string{"errtext", "nohttpmux", "sleeptest", "sqlcheck", "sqlclosecheck", "rowserrcheck", "testifyhelper"}, analyzerNames(t, p))
	assert.Equal(register.LoadModeTypesInfo, p.GetLoadMode())
	assert.False(errtext.IncludeTests)
	assert.True(sleeptest.HelperPackages)
	assert.False(sleeptest.Eventually)
}

func TestPluginSettings(t *testing.T) {
	assert := assert.New(t)
	t.Cleanup(func() {
		errtext.IncludeTests = false
		sleeptest.HelperPackages = true
		sleeptest.Eventually = false
	})
	p, err := New(map[string]any{
		"disable":   []any{"sleeptest", "nohttpmux"},
		"errtext":   map[string]any{"include-tests": true},
		"sleeptest": map[string]any{"helper-packages": false, "eventually": true},
	})
	require.NoError(t, err)
	assert.Equal([]string{"errtext", "sqlcheck", "sqlclosecheck", "rowserrcheck", "testifyhelper"}, analyzerNames(t, p))
	assert.True(errtext.IncludeTests)
	assert.False(sleeptest.HelperPackages)
	assert.True(sleeptest.Eventually)
}

func TestPluginRejectsUnknownSettings(t *testing.T) {
	assert := assert.New(t)
	_, err := New(map[string]any{"disable": []any{"nope"}})
	require.ErrorContains(t, err, `unknown analyzer "nope"`)

	_, err = New(map[string]any{"bogus": true})
	assert.ErrorContains(err, "kennlint settings")
}
