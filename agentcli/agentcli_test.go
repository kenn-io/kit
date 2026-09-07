package agentcli_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentcli"
)

func TestInteractiveResumePreservesConfiguredCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		agent    agentcli.Adapter
		expected []string
	}{
		{
			name:     "codex subcommand",
			agent:    agentcli.NewCodex([]string{"codex-custom", "--profile", "team"}),
			expected: []string{"codex-custom", "--profile", "team", "resume", "session-1"},
		},
		{
			name:     "claude flag",
			agent:    agentcli.NewClaude([]string{"claude-custom", "--setting-sources", "project"}),
			expected: []string{"claude-custom", "--setting-sources", "project", "--resume", "session-1"},
		},
		{
			name:     "pi flag",
			agent:    agentcli.NewPi([]string{"pi-custom", "--offline"}),
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
	invocation, err := agentcli.NewCodex(nil).Resume("thread-id", agentcli.Request{
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

	invocation, err := agentcli.NewClaude(nil).Start(agentcli.Request{
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

	invocation, err := agentcli.NewClaude(nil).Start(agentcli.Request{
		Mode:                agentcli.NonInteractive,
		DisableBuiltInTools: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"claude", "--print", "--tools", ""}, invocation.Argv)
}

func TestPiSchemaInvocation(t *testing.T) {
	t.Parallel()

	invocation, err := agentcli.NewPi(nil).Start(agentcli.Request{
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
			agent:   agentcli.NewCodex(nil),
			request: agentcli.Request{Mode: agentcli.NonInteractive, OutputFormat: agentcli.OutputJSON},
			option:  "output format",
		},
		{
			name:    "claude sandbox",
			agent:   agentcli.NewClaude(nil),
			request: agentcli.Request{Sandbox: agentcli.SandboxReadOnly},
			option:  "sandbox",
		},
		{
			name:    "pi approval policy",
			agent:   agentcli.NewPi(nil),
			request: agentcli.Request{Approval: agentcli.ApprovalNever},
			option:  "approval mode",
		},
		{
			name:    "interactive stdin prompt",
			agent:   agentcli.NewCodex(nil),
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

	_, err := agentcli.NewCodex(nil).Resume("--last", agentcli.Request{})
	require.Error(t, err)
	var unsupported *agentcli.UnsupportedOptionError
	assert.NotErrorAs(t, err, &unsupported)
}

func TestCapabilitiesAreExplicitAndIndependent(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)

	codex := agentcli.NewCodex(nil)
	capabilities := codex.Capabilities()
	assert.True(capabilities.Resume)
	assert.True(capabilities.JSONSchemaPath)
	assert.False(capabilities.JSONSchemaInline)
	assert.Equal(agentcli.DisableHooksOnly, capabilities.DisableHooks)

	capabilities.Modes[0] = "changed"
	assert.Equal(agentcli.Interactive, codex.Capabilities().Modes[0])
}
