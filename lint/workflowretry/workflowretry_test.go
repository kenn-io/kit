package workflowretry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScan(t *testing.T) {
	for _, tt := range []struct{ script, message string }{
		{"curl URL", "use kit-download"},
		{"wget URL", "use kit-download"},
		{"kit-retry 3 5 curl URL", "use kit-download"},
		{">log curl URL", "use kit-download"},
		{"wget -qO- URL | tar xz", "use kit-download"},
		{"x=$(curl URL)", "use kit-download"},
		{"sudo curl URL", "use kit-download"},
		{"if ! curl URL; then exit 1; fi", "use kit-download"},
		{"apt-get update", "wrap in kit-retry"},
		{"go install x", "wrap in kit-retry"},
		{"go mod download", "wrap in kit-retry"},
		{"npm ci", "wrap in kit-retry"},
		{"pip3 install x", "wrap in kit-retry"},
		{"docker pull x", "wrap in kit-retry"},
		{"apk update", "wrap in kit-retry"},
		{"brew install x", "wrap in kit-retry"},
		{"env X=1 apt-get install x", "wrap in kit-retry"},
		{"# workflowretry: allow diagnostics\ncurl URL 2>&1", ""},
		{"kit-download URL dest", ""},
		{"kit-retry 3 5 sudo apt-get -y install x", ""},
		{"FOO=1 kit-retry 3 5 npm ci", ""},
		{"echo curl; command -v curl", ""},
		{"curl URL # workflowretry: allow health probe", ""},
		{"curl ${{ runner.temp }}/x", "use kit-download"},
		{"echo 'unterminated", "parse shell:"},
		{"echo curl\ncurl URL", "use kit-download"},
		{"'cu'\"rl\" URL", "use kit-download"},
		{"# workflowretry: allow \ncurl URL", "use kit-download"},
	} {
		t.Run(tt.script, func(t *testing.T) {
			src := "jobs:\n  test:\n    steps:\n      - run: |\n          " + strings.ReplaceAll(tt.script, "\n", "\n          ") + "\n"
			findings, err := Scan([]byte(src))
			require.NoError(t, err)
			if tt.message == "" {
				assert.Empty(t, findings)
				return
			}
			require.Len(t, findings, 1)
			assert.Contains(t, findings[0].Message, tt.message)
			assert.Equal(t, 4, findings[0].Line)
		})
	}
}

func TestScanShellsAndComposite(t *testing.T) {
	for _, src := range []string{
		"defaults: {run: {shell: pwsh}}\njobs: {test: {steps: [{run: curl URL}]}}",
		"jobs: {test: {defaults: {run: {shell: pwsh}}, steps: [{run: curl URL}]}}",
		"runs: {using: composite, steps: [{shell: bash, run: 'kit-download URL dest'}]}",
	} {
		findings, err := Scan([]byte(src))
		require.NoError(t, err)
		assert.Empty(t, findings)
	}
	findings, err := Scan([]byte("runs: {using: composite, steps: [{shell: bash, run: 'curl URL'}]}"))
	require.NoError(t, err)
	require.Len(t, findings, 1)
	assert.Equal(t, "composite", findings[0].Job)
}
