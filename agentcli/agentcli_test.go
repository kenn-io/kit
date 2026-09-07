package agentcli_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentcli"
)

func newCodex(t *testing.T, command agentcli.Command) agentcli.Adapter {
	t.Helper()
	agent, err := agentcli.NewCodex(command)
	require.NoError(t, err)
	return agent
}

func newClaude(t *testing.T, command agentcli.Command) agentcli.Adapter {
	t.Helper()
	agent, err := agentcli.NewClaude(command)
	require.NoError(t, err)
	return agent
}

func newPi(t *testing.T, command agentcli.Command) agentcli.Adapter {
	t.Helper()
	agent, err := agentcli.NewPi(command)
	require.NoError(t, err)
	return agent
}

func TestSupportedAgentNamesConstructAdapters(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	expected := []agentcli.Name{
		agentcli.Codex,
		agentcli.Claude,
		agentcli.Gemini,
		agentcli.Copilot,
		agentcli.OpenCode,
		agentcli.Cursor,
		agentcli.Kiro,
		agentcli.Kilo,
		agentcli.Droid,
		agentcli.Pi,
	}
	assert.Equal(expected, agentcli.Names())
	for _, name := range expected {
		agent, err := agentcli.New(name, agentcli.Command{})
		require.NoError(err)
		assert.Equal(name, agent.Name())
	}

	names := agentcli.Names()
	names[0] = "changed"
	assert.Equal(agentcli.Codex, agentcli.Names()[0])

	_, err := agentcli.New("unknown", agentcli.Command{})
	require.Error(err)
}

func TestInteractiveResumePreservesConfiguredCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		agent    agentcli.Adapter
		expected []string
	}{
		{
			name:     "codex subcommand",
			agent:    newCodex(t, agentcli.Command{Executable: "codex-custom", Options: []string{"--profile", "team"}}),
			expected: []string{"codex-custom", "--profile", "team", "resume", "session-1"},
		},
		{
			name:     "claude flag",
			agent:    newClaude(t, agentcli.Command{Executable: "claude-custom", Options: []string{"--setting-sources", "project"}}),
			expected: []string{"claude-custom", "--setting-sources", "project", "--resume", "session-1"},
		},
		{
			name:     "pi flag",
			agent:    newPi(t, agentcli.Command{Executable: "pi-custom", Options: []string{"--offline"}}),
			expected: []string{"pi-custom", "--offline", "--session", "session-1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			invocation, err := test.agent.Resume("session-1", agentcli.Request{})
			require.NoError(err)
			assert.Equal(test.expected, invocation.Argv)
			assert.Nil(invocation.Stdin)
		})
	}
}

func TestCodexNonInteractiveResume(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	prompt := "continue from the saved state"
	invocation, err := newCodex(t, agentcli.Command{}).Resume("thread-id", agentcli.Request{
		Mode:                  agentcli.NonInteractive,
		Prompt:                agentcli.Prompt{Source: agentcli.PromptStdin, Text: prompt},
		Model:                 "gpt-test",
		Reasoning:             agentcli.ReasoningXHigh,
		OutputFormat:          agentcli.OutputJSONL,
		Sandbox:               agentcli.SandboxReadOnly,
		Approval:              agentcli.ApprovalNever,
		DisableSkills:         true,
		DisableHooks:          true,
		DisableUserConfig:     true,
		DisableSessionStorage: true,
		ConfigOverrides:       []string{"feature.test=true"},
	})
	require.NoError(err)
	assert.Equal([]string{
		"codex", "exec", "resume",
		"-c", "feature.test=true",
		"--ignore-user-config",
		"-c", "skills.include_instructions=false",
		"--disable", "hooks",
		"--ephemeral",
		"--model", "gpt-test",
		"-c", `model_reasoning_effort="xhigh"`,
		"-c", `sandbox_mode="read-only"`,
		"-c", `approval_policy="never"`,
		"--json",
		"thread-id", "-",
	}, invocation.Argv)
	require.NotNil(invocation.Stdin)
	assert.Equal(prompt, *invocation.Stdin)
}

func TestClaudeNonInteractiveStructuredOutput(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	invocation, err := newClaude(t, agentcli.Command{}).Start(agentcli.Request{
		Mode:          agentcli.NonInteractive,
		Prompt:        agentcli.Prompt{Source: agentcli.PromptStdin, Text: "classify"},
		Model:         "sonnet",
		Reasoning:     agentcli.ReasoningHigh,
		OutputFormat:  agentcli.OutputJSONL,
		Schema:        agentcli.JSONSchema{Inline: `{"type":"object"}`},
		Approval:      agentcli.ApprovalNever,
		AllowedTools:  []string{"Read", "Glob"},
		DeniedTools:   []string{"Bash"},
		DisableSkills: true,
	})
	require.NoError(err)
	assert.Equal([]string{
		"claude", "--print", "--verbose", "--output-format", "stream-json",
		"--json-schema", `{"type":"object"}`,
		"--model", "sonnet",
		"--effort", "high",
		"--disable-slash-commands",
		"--permission-mode", "dontAsk",
		"--allowedTools", "Read,Glob",
		"--disallowedTools", "Bash",
	}, invocation.Argv)
	require.NotNil(invocation.Stdin)
	assert.Equal("classify", *invocation.Stdin)
}

func TestClaudeCanDisableAllBuiltInTools(t *testing.T) {
	t.Parallel()

	invocation, err := newClaude(t, agentcli.Command{}).Start(agentcli.Request{
		Mode:                agentcli.NonInteractive,
		DisableBuiltInTools: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "--print", "--tools", ""}, invocation.Argv)
}

func TestPiSchemaInvocation(t *testing.T) {
	t.Parallel()

	invocation, err := newPi(t, agentcli.Command{}).Start(agentcli.Request{
		Mode:                   agentcli.NonInteractive,
		Prompt:                 agentcli.Prompt{Source: agentcli.PromptArgument, Text: "classify", Files: []string{"prompt.md"}},
		Provider:               "test-provider",
		Model:                  "test-model",
		Reasoning:              agentcli.ReasoningMaximum,
		Schema:                 agentcli.JSONSchema{Inline: `{"type":"object"}`, Extension: "schema-extension", OutputPath: "result.json"},
		DisableBuiltInTools:    true,
		DisableSkills:          true,
		DisableHooks:           true,
		DisablePromptTemplates: true,
		DisableThemes:          true,
		DisableContextFiles:    true,
		DisableSessionStorage:  true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"pi",
		"--no-session",
		"--no-extensions",
		"--no-builtin-tools",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-context-files",
		"--extension", "schema-extension",
		"--json-schema", `{"type":"object"}`,
		"--json-output", "result.json",
		"--json-fallback", "none",
		"--print",
		"--provider", "test-provider",
		"--model", "test-model",
		"--thinking", "max",
		"@prompt.md", "classify",
	}, invocation.Argv)
	assert.Nil(t, invocation.Stdin)
}

func TestUnsupportedOptionsReturnTypedErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		agent   agentcli.Adapter
		request agentcli.Request
		option  string
	}{
		{
			name:    "codex single JSON document",
			agent:   newCodex(t, agentcli.Command{}),
			request: agentcli.Request{Mode: agentcli.NonInteractive, OutputFormat: agentcli.OutputJSON},
			option:  "output format",
		},
		{
			name:    "claude sandbox",
			agent:   newClaude(t, agentcli.Command{}),
			request: agentcli.Request{Sandbox: agentcli.SandboxReadOnly},
			option:  "sandbox",
		},
		{
			name:    "pi approval policy",
			agent:   newPi(t, agentcli.Command{}),
			request: agentcli.Request{Approval: agentcli.ApprovalNever},
			option:  "approval mode",
		},
		{
			name:    "interactive stdin prompt",
			agent:   newCodex(t, agentcli.Command{}),
			request: agentcli.Request{Prompt: agentcli.Prompt{Source: agentcli.PromptStdin, Text: "prompt"}},
			option:  "stdin prompt",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			_, err := test.agent.Start(test.request)
			var unsupported *agentcli.UnsupportedOptionError
			require.ErrorAs(err, &unsupported)
			assert.Equal(test.agent.Name(), unsupported.Agent)
			assert.Equal(test.option, unsupported.Option)
			assert.NotEmpty(unsupported.Hint)
		})
	}
}

func TestResumeRejectsOptionShapedSessionID(t *testing.T) {
	t.Parallel()

	_, err := newCodex(t, agentcli.Command{}).Resume("--last", agentcli.Request{})
	require.Error(t, err)
	var unsupported *agentcli.UnsupportedOptionError
	assert.NotErrorAs(t, err, &unsupported)
}

func TestCapabilitiesAreExplicitAndIndependent(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	codex := newCodex(t, agentcli.Command{})
	capabilities := codex.Capabilities()
	assert.True(capabilities.Resume)
	assert.True(capabilities.JSONSchemaPath)
	assert.False(capabilities.JSONSchemaInline)
	assert.Equal(agentcli.DisableHooksOnly, capabilities.DisableHooks)
	assert.Equal([]agentcli.ReasoningLevel{
		agentcli.ReasoningLow,
		agentcli.ReasoningMedium,
		agentcli.ReasoningHigh,
		agentcli.ReasoningXHigh,
		agentcli.ReasoningMaximum,
	}, capabilities.ReasoningLevels)

	capabilities.Modes[0] = "changed"
	capabilities.PromptSources[0] = "changed"
	capabilities.ReasoningLevels[0] = "changed"
	assert.Equal(agentcli.Interactive, codex.Capabilities().Modes[0])
	assert.Equal(agentcli.PromptArgument, codex.Capabilities().PromptSources[0])
	assert.Equal(agentcli.ReasoningLow, codex.Capabilities().ReasoningLevels[0])

	droid, err := agentcli.NewDroid(agentcli.Command{})
	require.NoError(t, err)
	droidCapabilities := droid.Capabilities()
	droidCapabilities.AutonomyLevels[0] = "changed"
	assert.Equal(agentcli.AutonomyLow, droid.Capabilities().AutonomyLevels[0])
}

func TestConfiguredCommandValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		new     func(agentcli.Command) (agentcli.Adapter, error)
		command agentcli.Command
		token   string
	}{
		{name: "codex prompt", new: agentcli.NewCodex, command: agentcli.Command{Options: []string{"old prompt"}}, token: "old prompt"},
		{name: "codex subcommand", new: agentcli.NewCodex, command: agentcli.Command{Options: []string{"exec"}}, token: "exec"},
		{name: "codex missing profile", new: agentcli.NewCodex, command: agentcli.Command{Options: []string{"--profile"}}, token: "--profile"},
		{name: "codex option cannot swallow subcommand-shaped flag", new: agentcli.NewCodex, command: agentcli.Command{Options: []string{"--profile", "--help"}}, token: "--profile"},
		{name: "codex unknown option", new: agentcli.NewCodex, command: agentcli.Command{Options: []string{"--future-flag"}}, token: "--future-flag"},
		{name: "claude resume", new: agentcli.NewClaude, command: agentcli.Command{Options: []string{"--resume", "old-session"}}, token: "--resume"},
		{name: "claude selector cannot become settings value", new: agentcli.NewClaude, command: agentcli.Command{Options: []string{"--settings", "--resume"}}, token: "--settings"},
		{name: "claude command", new: agentcli.NewClaude, command: agentcli.Command{Options: []string{"agents"}}, token: "agents"},
		{name: "claude optional arity", new: agentcli.NewClaude, command: agentcli.Command{Options: []string{"--debug", "api"}}, token: "--debug"},
		{name: "pi session", new: agentcli.NewPi, command: agentcli.Command{Options: []string{"--session", "old-session"}}, token: "--session"},
		{name: "pi prompt boundary", new: agentcli.NewPi, command: agentcli.Command{Options: []string{"--", "old prompt"}}, token: "--"},
		{name: "pi action", new: agentcli.NewPi, command: agentcli.Command{Options: []string{"install", "extension"}}, token: "install"},
		{name: "gemini resume", new: agentcli.NewGemini, command: agentcli.Command{Options: []string{"--resume", "old-session"}}, token: "--resume"},
		{name: "copilot missing model", new: agentcli.NewCopilot, command: agentcli.Command{Options: []string{"--model"}}, token: "--model"},
		{name: "opencode session", new: agentcli.NewOpenCode, command: agentcli.Command{Options: []string{"--session", "old-session"}}, token: "--session"},
		{name: "cursor prompt", new: agentcli.NewCursor, command: agentcli.Command{Options: []string{"old prompt"}}, token: "old prompt"},
		{name: "kilo session", new: agentcli.NewKilo, command: agentcli.Command{Options: []string{"--session", "old-session"}}, token: "--session"},
		{name: "kiro missing wrap", new: agentcli.NewKiro, command: agentcli.Command{Options: []string{"--wrap"}}, token: "--wrap"},
		{name: "droid session", new: agentcli.NewDroid, command: agentcli.Command{Options: []string{"--session-id", "old-session"}}, token: "--session-id"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			_, err := test.new(test.command)
			var invalid *agentcli.InvalidCommandError
			require.ErrorAs(err, &invalid)
			assert.Equal(test.token, invalid.Token)
			assert.NotEmpty(invalid.Reason)
			assert.NotEmpty(invalid.Hint)
		})
	}
}

func TestConfiguredOptionsPreserveArityAndOrdering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		new      func(agentcli.Command) (agentcli.Adapter, error)
		command  agentcli.Command
		mode     agentcli.Mode
		expected []string
	}{
		{
			name: "codex long inline and short separate values",
			new:  agentcli.NewCodex,
			command: agentcli.Command{Executable: "codex-custom", Options: []string{
				"--profile=team", "-c", "feature.test=true", "--add-dir", "-shared",
			}},
			expected: []string{"codex-custom", "--profile=team", "-c", "feature.test=true", "--add-dir", "-shared", "resume", "session-1"},
		},
		{
			name: "claude aliases and repeated options",
			new:  agentcli.NewClaude,
			command: agentcli.Command{Options: []string{
				"--setting-sources=project", "--plugin-dir", "one", "--plugin-dir", "-two",
			}},
			expected: []string{"claude", "--setting-sources=project", "--plugin-dir", "one", "--plugin-dir", "-two", "--resume", "session-1"},
		},
		{
			name:     "pi short flag and value",
			new:      agentcli.NewPi,
			command:  agentcli.Command{Options: []string{"-ne", "--tui-mode", "fullscreen", "--offline"}},
			expected: []string{"pi", "-ne", "--tui-mode", "fullscreen", "--offline", "--session", "session-1"},
		},
		{
			name:     "kiro chat options follow subcommand",
			new:      agentcli.NewKiro,
			command:  agentcli.Command{Executable: "kiro-custom", Options: []string{"--wrap", "never"}},
			mode:     agentcli.NonInteractive,
			expected: []string{"kiro-custom", "chat", "--wrap", "never", "--no-interactive", "--resume-id", "session-1"},
		},
		{
			name:     "droid exec options follow subcommand",
			new:      agentcli.NewDroid,
			command:  agentcli.Command{Executable: "droid-custom", Options: []string{"--append-system-prompt", "review only"}},
			mode:     agentcli.NonInteractive,
			expected: []string{"droid-custom", "exec", "--append-system-prompt", "review only", "--session-id", "session-1"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent, err := test.new(test.command)
			require.NoError(t, err)
			invocation, err := agent.Resume("session-1", agentcli.Request{Mode: test.mode})
			require.NoError(t, err)
			assert.Equal(t, test.expected, invocation.Argv)
		})
	}
}

func TestConfiguredOptionConflictsWithRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		new     func(agentcli.Command) (agentcli.Adapter, error)
		options []string
		request agentcli.Request
	}{
		{name: "codex model", new: agentcli.NewCodex, options: []string{"--model", "configured"}, request: agentcli.Request{Model: "requested"}},
		{name: "claude reasoning", new: agentcli.NewClaude, options: []string{"--effort", "high"}, request: agentcli.Request{Reasoning: agentcli.ReasoningXHigh}},
		{name: "pi provider", new: agentcli.NewPi, options: []string{"--provider", "configured"}, request: agentcli.Request{Provider: "requested"}},
		{name: "gemini output", new: agentcli.NewGemini, options: []string{"--output-format", "json"}, request: agentcli.Request{Mode: agentcli.NonInteractive, OutputFormat: agentcli.OutputJSONL}},
		{name: "copilot MCPs", new: agentcli.NewCopilot, options: []string{"--disable-builtin-mcps"}, request: agentcli.Request{Mode: agentcli.NonInteractive, DisableBuiltInMCPs: true}},
		{name: "opencode model", new: agentcli.NewOpenCode, options: []string{"--model", "configured"}, request: agentcli.Request{Mode: agentcli.NonInteractive, Model: "requested"}},
		{name: "cursor model", new: agentcli.NewCursor, options: []string{"--model", "configured"}, request: agentcli.Request{Mode: agentcli.NonInteractive, Model: "requested"}},
		{name: "kilo model", new: agentcli.NewKilo, options: []string{"--model", "configured"}, request: agentcli.Request{Mode: agentcli.NonInteractive, Model: "requested"}},
		{name: "kiro reasoning", new: agentcli.NewKiro, options: []string{"--effort", "high"}, request: agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningXHigh}},
		{name: "droid autonomy", new: agentcli.NewDroid, options: []string{"--auto", "low"}, request: agentcli.Request{Mode: agentcli.NonInteractive, Autonomy: agentcli.AutonomyMedium}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent, err := test.new(agentcli.Command{Options: test.options})
			require.NoError(t, err)
			_, err = agent.Start(test.request)
			var invalid *agentcli.InvalidCommandError
			require.ErrorAs(t, err, &invalid)
			assert.Contains(t, invalid.Reason, "conflicts")
		})
	}
}

func TestAdditionalRoboRevAgentInvocations(t *testing.T) {
	t.Parallel()

	prompt := "review this change"
	tests := []struct {
		name     string
		new      func(agentcli.Command) (agentcli.Adapter, error)
		resume   bool
		request  agentcli.Request
		expected []string
	}{
		{
			name:     "gemini",
			new:      agentcli.NewGemini,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptStdin, Text: prompt}, Model: "gemini-test", OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalNever},
			expected: []string{"gemini", "--output-format", "stream-json", "--resume", "session-1", "--model", "gemini-test", "--approval-mode", "plan", "--prompt", ""},
		},
		{
			name:     "copilot",
			new:      agentcli.NewCopilot,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptArgument, Text: prompt}, Model: "copilot-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalBypass, DeniedTools: []string{"write"}, DisableBuiltInMCPs: true, DisableContextFiles: true},
			expected: []string{"copilot", "--silent", "--allow-all-tools", "--stream", "off", "--output-format", "json", "--resume=session-1", "--model", "copilot-test", "--reasoning-effort", "xhigh", "--allow-all", "--deny-tool", "write", "--disable-builtin-mcps", "--no-custom-instructions", "--prompt", prompt},
		},
		{
			name:     "opencode",
			new:      agentcli.NewOpenCode,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptStdin, Text: prompt}, Model: "provider/model", OutputFormat: agentcli.OutputJSONL},
			expected: []string{"opencode", "run", "--format", "json", "--session", "session-1", "--model", "provider/model"},
		},
		{
			name:     "cursor",
			new:      agentcli.NewCursor,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptStdin, Text: prompt}, Model: "cursor-test", OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalNever},
			expected: []string{"agent", "--print", "--output-format", "stream-json", "--resume", "session-1", "--model", "cursor-test", "--mode", "plan"},
		},
		{
			name:     "kilo",
			new:      agentcli.NewKilo,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptStdin, Text: prompt}, Model: "provider/model", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Approval: agentcli.ApprovalBypass},
			expected: []string{"kilo", "run", "--format", "json", "--session", "session-1", "--model", "provider/model", "--auto", "--variant", "xhigh"},
		},
		{
			name:     "kiro",
			new:      agentcli.NewKiro,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptArgument, Text: prompt}, Reasoning: agentcli.ReasoningXHigh, Approval: agentcli.ApprovalBypass},
			expected: []string{"kiro-cli", "chat", "--no-interactive", "--resume-id", "session-1", "--effort", "xhigh", "--trust-all-tools", "--", prompt},
		},
		{
			name:     "droid",
			new:      agentcli.NewDroid,
			resume:   true,
			request:  agentcli.Request{Mode: agentcli.NonInteractive, Prompt: agentcli.Prompt{Source: agentcli.PromptStdin, Text: prompt}, Model: "droid-test", Reasoning: agentcli.ReasoningXHigh, OutputFormat: agentcli.OutputJSONL, Autonomy: agentcli.AutonomyMedium, DeniedTools: []string{"execute-cli"}, DisableSkills: true},
			expected: []string{"droid", "exec", "--session-id", "session-1", "--model", "droid-test", "--reasoning-effort", "xhigh", "--auto", "medium", "--disabled-tools", "execute-cli", "--disable-builtin-skills", "--output-format", "stream-json"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			require := require.New(t)
			agent, err := test.new(agentcli.Command{})
			require.NoError(err)
			var invocation agentcli.Invocation
			if test.resume {
				invocation, err = agent.Resume("session-1", test.request)
			} else {
				invocation, err = agent.Start(test.request)
			}
			require.NoError(err)
			assert.Equal(test.expected, invocation.Argv)
			if test.request.Prompt.Source == agentcli.PromptStdin {
				require.NotNil(invocation.Stdin)
				assert.Equal(prompt, *invocation.Stdin)
			} else {
				assert.Nil(invocation.Stdin)
			}
		})
	}
}

func TestReasoningXHighRemainsDistinctFromMaximum(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	require := require.New(t)

	tests := []struct {
		new            func(agentcli.Command) (agentcli.Adapter, error)
		xhigh, maximum string
	}{
		{agentcli.NewCodex, `model_reasoning_effort="xhigh"`, `model_reasoning_effort="max"`},
		{agentcli.NewClaude, "xhigh", "max"},
		{agentcli.NewPi, "xhigh", "max"},
		{agentcli.NewCopilot, "xhigh", "max"},
		{agentcli.NewKilo, "xhigh", "max"},
		{agentcli.NewKiro, "xhigh", "max"},
		{agentcli.NewDroid, "xhigh", "max"},
	}
	for _, test := range tests {
		agent, err := test.new(agentcli.Command{})
		require.NoError(err)
		xhigh, err := agent.Start(agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningXHigh})
		require.NoError(err)
		maximum, err := agent.Start(agentcli.Request{Mode: agentcli.NonInteractive, Reasoning: agentcli.ReasoningMaximum})
		require.NoError(err)
		assert.Contains(xhigh.Argv, test.xhigh, agent.Name())
		assert.Contains(maximum.Argv, test.maximum, agent.Name())
	}
}
