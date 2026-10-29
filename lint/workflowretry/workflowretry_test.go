package workflowretry

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScan(t *testing.T) {
	for _, tt := range []struct {
		script   string
		messages []string
	}{
		{"curl URL", []string{"curl on script line 1: use kit-download"}},
		{`c\url https://example.com/x`, []string{"curl on script line 1: use kit-download"}},
		{"wget URL", []string{"wget on script line 1: use kit-download"}},
		{"kit-retry 3 5 curl URL", []string{"curl on script line 1: use kit-download"}},
		{">log curl URL", []string{"curl on script line 1: use kit-download"}},
		{"wget -qO- URL | tar xz", []string{"wget on script line 1: use kit-download"}},
		{"x=$(curl URL)", []string{"curl on script line 1: use kit-download"}},
		{"sudo curl URL", []string{"curl on script line 1: use kit-download"}},
		{"/usr/bin/curl URL", []string{"curl on script line 1: use kit-download"}},
		{"command curl URL", []string{"curl on script line 1: use kit-download"}},
		{"kit-retry 3 5 exec curl URL", []string{"curl on script line 1: use kit-download"}},
		{"if ! curl URL; then exit 1; fi", []string{"curl on script line 1: use kit-download"}},
		{"apt-get update", []string{"apt-get on script line 1: wrap in kit-retry"}},
		{"sudo apt-get -y install jq", []string{"apt-get on script line 1: wrap in kit-retry"}},
		{"apt-get -qq update", []string{"apt-get on script line 1: wrap in kit-retry"}},
		{"sudo apt-get -o APT::Update::Error-Mode=any update", []string{"apt-get on script line 1: wrap in kit-retry"}},
		{"go install x", []string{"go on script line 1: wrap in kit-retry"}},
		{"go mod download", []string{"go on script line 1: wrap in kit-retry"}},
		{"npm ci", []string{"npm on script line 1: wrap in kit-retry"}},
		{"npm --prefix web ci", []string{"npm on script line 1: wrap in kit-retry"}},
		{"pip3 install x", []string{"pip3 on script line 1: wrap in kit-retry"}},
		{"python -m pip install x", []string{"pip on script line 1: wrap in kit-retry"}},
		{"python3 -m pip install x", []string{"pip on script line 1: wrap in kit-retry"}},
		{"docker pull x", []string{"docker on script line 1: wrap in kit-retry"}},
		{"apk update", []string{"apk on script line 1: wrap in kit-retry"}},
		{"brew install x", []string{"brew on script line 1: wrap in kit-retry"}},
		{"env X=1 apt-get install x", []string{"apt-get on script line 1: wrap in kit-retry"}},
		{"# workflowretry: allow diagnostics\ncurl URL 2>&1", nil},
		{"kit-download URL dest", nil},
		{"kit-retry 3 5 sudo apt-get -y install x", nil},
		{"FOO=1 kit-retry 3 5 npm ci", nil},
		{"echo curl; command -v curl", nil},
		{"go test -run install", nil},
		{"curl URL # workflowretry: allow health probe", nil},
		{"curl ${{ runner.temp }}/x", []string{"curl on script line 1: use kit-download"}},
		{"if ($env:CI) { Write-Host 'CI' }", nil},
		{"echo curl\ncurl URL", []string{"curl on script line 2: use kit-download"}},
		{"'cu'\"rl\" URL", []string{"curl on script line 1: use kit-download"}},
		{"# workflowretry: allow \ncurl URL", []string{"curl on script line 2: use kit-download"}},
		{"apt-get update\napt-get install -y jq", []string{"apt-get on script line 1: wrap in kit-retry", "apt-get on script line 2: wrap in kit-retry"}},
		{"curl URL\nwget URL", []string{"curl on script line 1: use kit-download", "wget on script line 2: use kit-download"}},
	} {
		t.Run(tt.script, func(t *testing.T) {
			src := "jobs:\n  test:\n    steps:\n      - run: |\n          " + strings.ReplaceAll(tt.script, "\n", "\n          ") + "\n"
			findings, err := Scan([]byte(src))
			require.NoError(t, err)
			require.Len(t, findings, len(tt.messages))
			for i, finding := range findings {
				assert.Equal(t, tt.messages[i], finding.Message)
				assert.Equal(t, 4, finding.Line)
				assert.Equal(t, 9, finding.Column)
			}
		})
	}
}

func TestScanShellsAndComposite(t *testing.T) {
	for _, src := range []string{
		"defaults: {run: {shell: pwsh}}\njobs: {test: {steps: [{run: curl URL}]}}",
		"jobs: {test: {defaults: {run: {shell: pwsh}}, steps: [{run: curl URL}]}}",
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
