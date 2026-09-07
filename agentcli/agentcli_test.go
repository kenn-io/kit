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
		Reasoning:             agentcli.ReasoningMaximum,
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
		"--thinking", "high",
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

	capabilities.Modes[0] = "changed"
	assert.Equal(agentcli.Interactive, codex.Capabilities().Modes[0])
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			agent, err := test.new(test.command)
			require.NoError(t, err)
			invocation, err := agent.Resume("session-1", agentcli.Request{})
			require.NoError(t, err)
			assert.Equal(t, test.expected, invocation.Argv)
		})
	}
}

func TestConfiguredOptionConflictsWithRequest(t *testing.T) {
	t.Parallel()

	agent, err := agentcli.NewCodex(agentcli.Command{Options: []string{"--model", "configured"}})
	require.NoError(t, err)
	_, err = agent.Start(agentcli.Request{Model: "requested"})
	var invalid *agentcli.InvalidCommandError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Reason, "conflicts")
}
