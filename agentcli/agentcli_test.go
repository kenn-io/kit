package agentcli_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentcli"
)

func mustAgent(t *testing.T, name agentcli.Name, command agentcli.Command) agentcli.Adapter {
	t.Helper()
	agent, err := agentcli.New(name, command)
	require.NoError(t, err)
	return agent
}

func TestSupportedAgents(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	want := []agentcli.Name{agentcli.Codex, agentcli.Claude, agentcli.Gemini, agentcli.Copilot, agentcli.OpenCode, agentcli.Cursor, agentcli.Kiro, agentcli.Kilo, agentcli.Droid, agentcli.Pi}
	assert.Equal(want, agentcli.Names())
	for _, name := range want {
		assert.Equal(name, mustAgent(t, name, agentcli.Command{}).Name())
	}
	names := agentcli.Names()
	names[0] = "changed"
	assert.Equal(agentcli.Codex, agentcli.Names()[0])
	_, err := agentcli.New("unknown", agentcli.Command{})
	require.Error(t, err)
}

func TestInvocationContracts(t *testing.T) {
	t.Parallel()
	prompt := "review this change"
	tests := []struct {
		name    agentcli.Name
		session string
		request agentcli.Request
		want    []string
	}{
		{agentcli.Codex, "thread-id", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "gpt-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Sandbox: agentcli.SandboxReadOnly, Approval: agentcli.ApprovalNever, DisableSkills: true, DisableHooks: true, DisableUserConfig: true, DisableSessionStorage: true, ConfigOverrides: []string{"feature.test=true"}}, []string{"codex", "exec", "resume", "-c", "feature.test=true", "--ignore-user-config", "-c", "skills.include_instructions=false", "--disable", "hooks", "--ephemeral", "--model", "gpt-test", "-c", `model_reasoning_effort="xhigh"`, "-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`, "--json", "thread-id", "-"}},
		{agentcli.Claude, "", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "sonnet", Reasoning: agentcli.ReasoningHigh, OutputFormat: agentcli.OutputJSONL, Schema: agentcli.JSONSchema{Inline: `{"type":"object"}`}, Approval: agentcli.ApprovalNever, AllowedTools: []string{"Read", "Glob"}, DeniedTools: []string{"Bash"}, DisableSkills: true}, []string{"claude", "--print", "--verbose", "--output-format", "stream-json", "--json-schema", `{"type":"object"}`, "--model", "sonnet", "--effort", "high", "--disable-slash-commands", "--permission-mode", "dontAsk", "--allowedTools", "Read,Glob", "--disallowedTools", "Bash"}},
		{agentcli.Gemini, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "gemini-test", OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalNever}, []string{"gemini", "--output-format", "stream-json", "--resume", "session-1", "--model", "gemini-test", "--approval-mode", "plan", "--prompt", ""}},
		{agentcli.Copilot, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "copilot-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalBypass, DeniedTools: []string{"write"}, DisableBuiltInMCPs: true, DisableContextFiles: true}, []string{"copilot", "--silent", "--allow-all-tools", "--stream", "off", "--output-format", "json", "--resume=session-1", "--model", "copilot-test", "--reasoning-effort", "xhigh", "--allow-all", "--deny-tool", "write", "--disable-builtin-mcps", "--no-custom-instructions", "--prompt", prompt}},
		{agentcli.OpenCode, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "provider/model", OutputFormat: agentcli.OutputJSONL}, []string{"opencode", "run", "--format", "json", "--session", "session-1", "--model", "provider/model"}},
		{agentcli.Cursor, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "cursor-test", OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalNever}, []string{"agent", "--print", "--output-format", "stream-json", "--resume", "session-1", "--model", "cursor-test", "--mode", "plan"}},
		{agentcli.Kiro, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Reasoning: agentcli.ReasoningXHigh, Approval: agentcli.ApprovalBypass}, []string{"kiro-cli", "chat", "--no-interactive", "--resume-id", "session-1", "--effort", "xhigh", "--trust-all-tools", "--", prompt}},
		{agentcli.Kilo, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "provider/model", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalBypass}, []string{"kilo", "run", "--format", "json", "--session", "session-1", "--model", "provider/model", "--auto", "--variant", "xhigh"}},
		{agentcli.Droid, "session-1", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: prompt}, Model: "droid-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Autonomy: agentcli.AutonomyMedium, DeniedTools: []string{"execute-cli"}, DisableSkills: true}, []string{"droid", "exec", "--session-id", "session-1", "--model", "droid-test", "--reasoning-effort", "xhigh", "--auto", "medium", "--disabled-tools", "execute-cli", "--disable-builtin-skills", "--output-format", "stream-json"}},
		{agentcli.Pi, "", agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Text: "--classify", Files: []string{"prompt.md"}}, Provider: "test-provider", Model: "test-model", Reasoning: agentcli.ReasoningMaximum, Schema: agentcli.JSONSchema{Inline: `{"type":"object"}`, Extension: "schema-extension", OutputPath: "result.json"}, DisableBuiltInTools: true, DisableSkills: true, DisableHooks: true, DisablePromptTemplates: true, DisableThemes: true, DisableContextFiles: true, DisableSessionStorage: true}, []string{"pi", "--no-session", "--no-extensions", "--no-builtin-tools", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--extension", "schema-extension", "--json-schema", `{"type":"object"}`, "--json-output", "result.json", "--json-fallback", "none", "--print", "--provider", "test-provider", "--model", "test-model", "--thinking", "max", "--", "@prompt.md", "--classify"}},
	}
	for _, test := range tests {
		t.Run(string(test.name), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			agent := mustAgent(t, test.name, agentcli.Command{})
			var got agentcli.Invocation
			var err error
			if test.session == "" {
				got, err = agent.Start(test.request)
			} else {
				got, err = agent.Resume(test.session, test.request)
			}
			require.NoError(err)
			assert.Equal(test.want, got.Argv)
			if test.name == agentcli.Codex || test.name == agentcli.Claude || test.name == agentcli.Gemini || test.name == agentcli.OpenCode || test.name == agentcli.Cursor || test.name == agentcli.Kilo || test.name == agentcli.Droid {
				require.NotNil(got.Stdin)
				assert.Equal(test.request.Prompt.Text, *got.Stdin)
			} else {
				assert.Nil(got.Stdin)
			}
		})
	}
}

func TestInteractiveResumePreservesConfiguredOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    agentcli.Name
		command agentcli.Command
		want    []string
	}{
		{agentcli.Codex, agentcli.Command{Executable: "codex-custom", Options: []string{"--full-auto", "--profile", "team"}}, []string{"codex-custom", "--full-auto", "--profile", "team", "resume", "session-1"}},
		{agentcli.Claude, agentcli.Command{Executable: "claude-custom", Options: []string{"--setting-sources", "project"}}, []string{"claude-custom", "--setting-sources", "project", "--resume", "session-1"}},
		{agentcli.Pi, agentcli.Command{Executable: "pi-custom", Options: []string{"--offline"}}, []string{"pi-custom", "--offline", "--session", "session-1"}},
	}
	for _, test := range tests {
		got, err := mustAgent(t, test.name, test.command).Resume("session-1", agentcli.Request{})
		require.NoError(t, err)
		assert.Equal(t, test.want, got.Argv)
	}
}

func TestUnsupportedRequestsReturnTypedErrors(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	tests := []struct {
		name    agentcli.Name
		request agentcli.Request
		option  string
	}{
		{agentcli.Codex, agentcli.Request{Mode: agentcli.NonInteractive, OutputFormat: agentcli.OutputJSON}, "output format"},
		{agentcli.Claude, agentcli.Request{Sandbox: agentcli.SandboxReadOnly}, "sandbox"},
		{agentcli.Pi, agentcli.Request{Approval: agentcli.ApprovalNever}, "approval mode"},
	}
	for _, test := range tests {
		_, err := mustAgent(t, test.name, agentcli.Command{}).Start(test.request)
		var unsupported *agentcli.UnsupportedOptionError
		require.ErrorAs(err, &unsupported)
		assert.Equal(test.option, unsupported.Option)
		assert.NotEmpty(unsupported.Hint)
	}
	_, err := mustAgent(t, agentcli.Codex, agentcli.Command{}).Resume("--last", agentcli.Request{})
	require.Error(err)
}

func TestCapabilitiesAreExplicitAndIndependent(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	codex := mustAgent(t, agentcli.Codex, agentcli.Command{})
	got := codex.Capabilities()
	assert.True(got.Resume)
	assert.True(got.JSONSchemaPath)
	assert.False(got.JSONSchemaInline)
	assert.Equal(agentcli.DisableHooksOnly, got.DisableHooks)
	assert.Equal([]agentcli.ReasoningLevel{agentcli.ReasoningLow, agentcli.ReasoningMedium, agentcli.ReasoningHigh, agentcli.ReasoningXHigh, agentcli.ReasoningMaximum}, got.ReasoningLevels)
	got.Modes[0], got.ReasoningLevels[0] = "changed", "changed"
	assert.Equal(agentcli.Interactive, codex.Capabilities().Modes[0])
	assert.Equal(agentcli.ReasoningLow, codex.Capabilities().ReasoningLevels[0])
}

func TestConfiguredOptionsKeepTheirArityAndOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    agentcli.Name
		command agentcli.Command
		mode    agentcli.Mode
		want    []string
	}{
		{agentcli.Codex, agentcli.Command{Executable: "codex-custom", Options: []string{"--profile=team", "-c", "feature.test=true", "--add-dir", "-shared"}}, "", []string{"codex-custom", "--profile=team", "-c", "feature.test=true", "--add-dir", "-shared", "resume", "session-1"}},
		{agentcli.Claude, agentcli.Command{Options: []string{"--setting-sources=project", "--plugin-dir", "one", "--plugin-dir", "-two"}}, "", []string{"claude", "--setting-sources=project", "--plugin-dir", "one", "--plugin-dir", "-two", "--resume", "session-1"}},
		{agentcli.Pi, agentcli.Command{Options: []string{"-ne", "--tui-mode", "fullscreen", "--offline"}}, "", []string{"pi", "-ne", "--tui-mode", "fullscreen", "--offline", "--session", "session-1"}},
		{agentcli.Kiro, agentcli.Command{Executable: "kiro-custom", Options: []string{"--wrap", "never"}}, agentcli.NonInteractive, []string{"kiro-custom", "chat", "--wrap", "never", "--no-interactive", "--resume-id", "session-1"}},
		{agentcli.Droid, agentcli.Command{Executable: "droid-custom", Options: []string{"--append-system-prompt", "review only"}}, agentcli.NonInteractive, []string{"droid-custom", "exec", "--append-system-prompt", "review only", "--session-id", "session-1"}},
	}
	for _, test := range tests {
		got, err := mustAgent(t, test.name, test.command).Resume("session-1", agentcli.Request{Mode: test.mode})
		require.NoError(t, err)
		assert.Equal(t, test.want, got.Argv)
	}
}

func TestConfiguredArgumentsPassThrough(t *testing.T) {
	t.Parallel()
	for _, name := range agentcli.Names() {
		t.Run(string(name), func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			options := []string{"--future-flag", "value with spaces", "--model", "configured", "--", "", "operand"}
			request := agentcli.Request{Mode: agentcli.NonInteractive}
			if name != agentcli.Kiro {
				request.Model = "requested"
			}
			agent := mustAgent(t, name, agentcli.Command{Executable: "custom-worker", Options: options})
			for _, resume := range []bool{false, true} {
				var got agentcli.Invocation
				var err error
				if resume {
					got, err = agent.Resume("session-1", request)
				} else {
					got, err = agent.Start(request)
				}
				require.NoError(err)
				assert.Equal("custom-worker", got.Argv[0])
				start := 1
				if name == agentcli.Kiro || name == agentcli.Droid {
					start++ // These CLIs put configured arguments after their subcommand.
				}
				assert.Equal(options, got.Argv[start:start+len(options)])
				if request.Model != "" {
					assert.Contains(got.Argv[start+len(options):], "requested")
				}
			}
		})
	}
}

func TestXHighAndMaximumStayDistinct(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)
	tests := []struct {
		name           agentcli.Name
		xhigh, maximum string
	}{
		{agentcli.Codex, `model_reasoning_effort="xhigh"`, `model_reasoning_effort="max"`},
		{agentcli.Claude, "xhigh", "max"}, {agentcli.Pi, "xhigh", "max"}, {agentcli.Copilot, "xhigh", "max"},
		{agentcli.Kilo, "xhigh", "max"}, {agentcli.Kiro, "xhigh", "max"}, {agentcli.Droid, "xhigh", "max"},
	}
	for _, test := range tests {
		agent := mustAgent(t, test.name, agentcli.Command{})
		xhigh, err := agent.Start(agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningXHigh})
		require.NoError(err)
		maximum, err := agent.Start(agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningMaximum})
		require.NoError(err)
		assert.Contains(xhigh.Argv, test.xhigh)
		assert.Contains(maximum.Argv, test.maximum)
	}
}

func TestClaudeCanDisableAllBuiltInTools(t *testing.T) {
	got, err := mustAgent(t, agentcli.Claude, agentcli.Command{}).Start(agentcli.Request{Mode: agentcli.NonInteractive, DisableBuiltInTools: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "--print", "--tools", ""}, got.Argv)
}
