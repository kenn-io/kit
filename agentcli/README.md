# Agent CLI command construction

`agentcli` builds argument vectors for coding-agent CLIs. It owns agent-specific
flag placement, configured-option grammar and arity, start and resume forms,
prompt delivery, and rejection of unsupported options. It does not execute
commands, inspect installed versions, resolve session files, or manage
terminals and persistent state.

Constructors accept an executable separately from configured options and
validate the options immediately. Configured options may not contain a prompt,
subcommand, session selector, `--`, or an option with ambiguous arity. Put every
prompt in `Request.Prompt`; use `Resume` for a saved session identity.

## Consumer examples

Forge can preserve a configured interactive command and resume without sending
the original prompt again:

```go
agent, err := agentcli.NewCodex(agentcli.Command{
	Executable: "codex",
	Options:    []string{"--profile", "forge"},
})
if err != nil {
	return err
}
invocation, err := agent.Resume(sessionID, agentcli.Request{})
// invocation.Argv: codex --profile forge resume <sessionID>
```

RoboRev can request a noninteractive event stream with explicit safety and
prompt transport:

```go
prompt := agentcli.Prompt{Source: agentcli.PromptStdin, Text: reviewPrompt}
agent, err := agentcli.NewCodex(agentcli.Command{Executable: configuredExecutable})
if err != nil {
	return err
}
invocation, err := agent.Start(agentcli.Request{
	Mode:         agentcli.NonInteractive,
	Prompt:       prompt,
	OutputFormat: agentcli.OutputJSONL,
	Sandbox:      agentcli.SandboxReadOnly,
	Approval:     agentcli.ApprovalNever,
})
// Pass invocation.Argv[0], invocation.Argv[1:], and *invocation.Stdin to os/exec.
```

Call `Capabilities` before presenting options in a UI. `Start` and `Resume`
still validate every request and return `UnsupportedOptionError` when an option
cannot keep its requested meaning. Constructors return `InvalidCommandError`
for unsafe configured command shapes. A request also returns that error when it
would duplicate a configured singleton option, such as a model or session
policy; remove one of the two settings instead of relying on CLI precedence.

## Capability matrix

| Capability | Codex | Claude Code | Pi |
| --- | --- | --- | --- |
| Interactive and noninteractive | yes | yes | yes |
| Resume by caller-supplied identity | `resume ID` or `exec resume ID` | `--resume ID` | `--session ID` |
| Output | text, JSONL | text, JSON, stream JSONL | text, JSONL |
| JSON Schema | schema file | inline schema | inline schema through an explicit extension and output file |
| Model and reasoning | yes | yes | yes |
| Provider | configured options | configured options | `--provider` |
| Sandbox | read-only, workspace-write, full access | no filesystem sandbox flag | no sandbox flag |
| Approval policy | on-request, never, bypass | manual, dontAsk, bypass | no tool-approval policy |
| Tool lists | no | allow, deny, disable built-ins | allow, deny, disable built-ins |
| Skill paths | no | no | yes |
| Disable skills | suppress skill instructions | disable slash commands | disable discovery |
| Disable hooks | hooks feature only | safe mode disables all customizations | disable extension discovery |
| Disable session storage | noninteractive | noninteractive | yes |
| Disable user config | noninteractive | configured options | configured options |
| Config overrides | `-c` | configured options | configured options |

The adapters reflect these CLI contracts:

- [Codex noninteractive mode](https://learn.chatgpt.com/docs/non-interactive-mode)
- [Codex configuration reference](https://developers.openai.com/codex/config-reference)
- [Claude Code CLI reference](https://code.claude.com/docs/en/cli-reference)
- [Pi README](https://github.com/earendil-works/pi/tree/main/packages/coding-agent)

The first consumer migrations should replace Forge's temporary command-option
validator together with its interactive resume switch; Forge should pass its
configured executable and option slice directly to the matching constructor.
RoboRev should replace its Codex, Claude, and Pi argument builders while keeping
stream parsing, installed-version capability probes, environment filtering, Pi
session-file lookup, and process lifecycle code. Keeping command-shape parsing
in either consumer would create a second grammar that can drift from these
adapters.
