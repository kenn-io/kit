package agenthook

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgyInstallWritesNamedGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	result, err := Install(AgentAgy, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks: []Hook{
			{Event: EventSessionStart},
			{Event: EventUserPromptSubmit, Timeout: 2 * time.Second},
			{Event: EventPreToolUse},
			{Event: EventPreToolUse, Matcher: ToolBash},
			{Event: EventStop},
		},
	})

	require.NoError(t, err)
	assert.True(t, result.Changed)
	command := `"type":"command","command":"/opt/hook --source shared-agent-hook-test"`
	assert.JSONEq(t, `{"--source shared-agent-hook-test":{
  "SessionStart":[{`+command+`}],
  "PreInvocation":[{`+command+`,"timeout":2}],
  "PreToolUse":[
    {"matcher":"*","hooks":[{`+command+`}]},
    {"matcher":"run_command","hooks":[{`+command+`}]}
  ],
  "Stop":[{`+command+`}]
}}`, string(result.Data))
}

func TestAgyReinstallAndUninstallKeepOtherHooks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	other := `{"type":"command","command":"/opt/other"}`
	require.NoError(t, os.WriteFile(path, []byte(`{
  "other":{"enabled":false,"Stop":[`+other+`]},
  "mixed":{
    "Stop":[`+other+`,{"type":"command","command":"/old/hook `+testMarker+`"}],
    "PreToolUse":[{"matcher":"*","hooks":[`+other+`,{"type":"command","command":"/old/hook `+testMarker+`"}]}]
  }
}`), 0o600))
	opts := InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop}},
	}

	_, err := Install(AgentAgy, opts)
	require.NoError(t, err)
	again, err := PlanInstall(AgentAgy, opts)
	require.NoError(t, err)
	assert.False(t, again.Changed)
	removed, err := Uninstall(AgentAgy, path, testMarker)
	require.NoError(t, err)

	assert.True(t, removed.Changed)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{
  "other":{"enabled":false,"Stop":[`+other+`]},
  "mixed":{"Stop":[`+other+`],"PreToolUse":[{"matcher":"*","hooks":[`+other+`]}]}
}`, string(data))
}

func TestAgyRejectsMatcherOnLifecycleEvent(t *testing.T) {
	_, err := PlanInstall(AgentAgy, InstallOptions{
		ConfigPath: filepath.Join(t.TempDir(), "hooks.json"),
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop, Matcher: ToolBash}},
	})

	require.ErrorContains(t, err, "only supports matchers on PreToolUse")
}

func TestAgyReinstallKeepsADisabledGroupDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"--source shared-agent-hook-test":{
  "enabled":false,
  "Stop":[{"type":"command","command":"/opt/hook `+testMarker+`"}]
}}`), 0o600))

	result, err := PlanInstall(AgentAgy, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/hook " + testMarker,
		Marker:     testMarker,
		Hooks:      []Hook{{Event: EventStop}},
	})

	require.NoError(t, err)
	assert.False(t, result.Changed)
	assert.JSONEq(t, `{"--source shared-agent-hook-test":{
  "enabled":false,
  "Stop":[{"type":"command","command":"/opt/hook --source shared-agent-hook-test"}]
}}`, string(result.Data))
}

func TestAgyAppsWithSimilarMarkersKeepTheirOwnEnabledSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"--source app.one":{
  "enabled":false,
  "Stop":[{"type":"command","command":"/opt/one --source app.one"}]
}}`), 0o600))

	result, err := PlanInstall(AgentAgy, InstallOptions{
		ConfigPath: path,
		Command:    "/opt/two --source app-one",
		Marker:     "--source app-one",
		Hooks:      []Hook{{Event: EventStop}},
	})

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "--source app.one":{"enabled":false,"Stop":[{"type":"command","command":"/opt/one --source app.one"}]},
  "--source app-one":{"Stop":[{"type":"command","command":"/opt/two --source app-one"}]}
}`, string(result.Data))
}

type sessionStartHandler struct {
	NoopHandler
	output SessionStartOutput
}

func (h sessionStartHandler) SessionStart(context.Context, SessionStartInput) (SessionStartOutput, error) {
	return h.output, nil
}

type agyRecorder struct {
	NoopHandler
	prompt *UserPromptSubmitInput
	tool   *PreToolUseInput
	stop   *StopInput
}

func (h *agyRecorder) UserPromptSubmit(_ context.Context, input UserPromptSubmitInput) (UserPromptSubmitOutput, error) {
	h.prompt = &input
	return UserPromptSubmitOutput{}, nil
}

func (h *agyRecorder) PreToolUse(_ context.Context, input PreToolUseInput) (PreToolUseOutput, error) {
	h.tool = &input
	return PreToolUseOutput{}, nil
}

func (h *agyRecorder) Stop(_ context.Context, input StopInput) (StopOutput, error) {
	h.stop = &input
	return StopOutput{}, nil
}

// The payloads follow agy 1.5's hook input, with synthetic paths.
func TestHandleNormalizesAgyPayloads(t *testing.T) {
	common := `"conversationId":"c-1","cwd":"/work","transcriptPath":"/work/t.jsonl",` +
		`"modelName":"gemini-pro","lastUserInput":"run the tests",`
	tests := []struct {
		name    string
		payload string
		output  string
		check   func(*testing.T, *agyRecorder)
	}{
		{
			name:    "pre invocation",
			payload: `{` + common + `"hookEventName":"PreInvocation","invocationNum":0,"initialNumSteps":1}`,
			output:  `{}`,
			check: func(t *testing.T, h *agyRecorder) {
				t.Helper()
				require.NotNil(t, h.prompt)
				assert.Equal(t, "c-1", h.prompt.SessionID)
				assert.Equal(t, "/work/t.jsonl", h.prompt.TranscriptPath)
				assert.Equal(t, "run the tests", h.prompt.Prompt)
			},
		},
		{
			name: "pre tool use",
			payload: `{` + common + `"hookEventName":"PreToolUse","stepIdx":3,` +
				`"toolCall":{"name":"run_command","args":{"CommandLine":"go test"}}}`,
			output: `{"decision":"allow"}`,
			check: func(t *testing.T, h *agyRecorder) {
				t.Helper()
				require.NotNil(t, h.tool)
				assert.Equal(t, ToolBash, h.tool.ToolName)
				assert.JSONEq(t, `{"CommandLine":"go test"}`, string(h.tool.ToolInput))
			},
		},
		{
			name: "stop",
			payload: `{` + common + `"hookEventName":"Stop","executionNum":0,` +
				`"terminationReason":"NO_TOOL_CALL","error":"","fullyIdle":true}`,
			output: `{"decision":"allow"}`,
			check: func(t *testing.T, h *agyRecorder) {
				t.Helper()
				require.NotNil(t, h.stop)
				assert.Equal(t, "c-1", h.stop.SessionID)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			handler := &agyRecorder{}
			err := Handle(t.Context(), AgentAgy, strings.NewReader(test.payload), &output, handler)

			require.NoError(t, err)
			assert.JSONEq(t, test.output, output.String())
			test.check(t, handler)
		})
	}
}

func TestHandleEncodesAgyControl(t *testing.T) {
	tool := `{"conversationId":"c-1","hookEventName":"PreToolUse",` +
		`"toolCall":{"name":"run_command","args":{"CommandLine":"rm -rf /"}}}`
	invocation := `{"conversationId":"c-1","hookEventName":"PreInvocation","lastUserInput":"go"}`
	stop := `{"conversationId":"c-1","hookEventName":"Stop"}`
	tests := []struct {
		name    string
		payload string
		handler Handler
		output  string
	}{
		{
			name:    "deny tool",
			payload: tool,
			handler: preToolHandler{output: PreToolUseOutput{
				PermissionDecision: PermissionDecisionDeny, PermissionDecisionReason: "destructive",
			}},
			output: `{"decision":"deny","reason":"destructive"}`,
		},
		{
			name:    "approve tool",
			payload: tool,
			handler: preToolHandler{output: PreToolUseOutput{PermissionDecision: PermissionDecisionAllow}},
			output:  `{"decision":"auto_approve"}`,
		},
		{
			name:    "block prompt",
			payload: invocation,
			handler: promptHandler{output: UserPromptSubmitOutput{Decision: DecisionBlock, Reason: "policy"}},
			output:  `{"terminationBehavior":"terminate","injectSteps":[{"errorMessage":{"userMessage":"policy"}}]}`,
		},
		{
			name:    "session context",
			payload: `{"conversationId":"c-1","hookEventName":"SessionStart"}`,
			handler: sessionStartHandler{output: SessionStartOutput{AdditionalContext: "use the staging cluster"}},
			output:  `{"injectSteps":[{"systemMessage":{"systemMessage":"use the staging cluster"}}]}`,
		},
		{
			name:    "keep going",
			payload: stop,
			handler: stopHandler{output: StopOutput{Decision: DecisionBlock, Reason: "tests fail"}},
			output:  `{"decision":"continue","reason":"tests fail"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			err := Handle(t.Context(), AgentAgy, strings.NewReader(test.payload), &output, test.handler)

			require.NoError(t, err)
			assert.JSONEq(t, test.output, output.String())
		})
	}
}

func TestHandleRejectsUnsupportedAgyControl(t *testing.T) {
	var output bytes.Buffer
	handler := preToolHandler{output: PreToolUseOutput{UpdatedInput: []byte(`{"CommandLine":"true"}`)}}

	err := Handle(t.Context(), AgentAgy, strings.NewReader(
		`{"conversationId":"c-1","hookEventName":"PreToolUse","toolCall":{"name":"view_file","args":{}}}`,
	), &output, handler)

	require.ErrorContains(t, err, "unsupported PreToolUse output fields")
	assert.Empty(t, output.String())
}
