// Package agenthook installs command hooks into supported agent harnesses and
// normalizes their input for Claude Code-style hook handlers.
//
// Its public event and matcher vocabulary follows Claude Code. Agent profiles
// translate that vocabulary to the native config path, event names, tool names,
// and file format used by each harness. This lets applications describe one set
// of lifecycle hooks while support for new agents stays centralized in kit.
// Profiles are provided for Claude Code, Codex, GitHub Copilot CLI, Cursor,
// Factory Droid, Gemini CLI, Hermes Agent, Muse Code, OpenCode, Pi, and Qwen
// Code. Pi and
// OpenCode have no command-hook config, so their profiles write a kit-owned
// module that runs the registered commands. The Pi module reports only from
// Pi's interactive terminal, so print, json, and rpc runs, subagents included,
// stay silent. Its SessionEnd fires only when another session replaces the
// current one (new, resume, or fork), never on quit or reload. The module
// reports an event only once Pi has saved the session file, so every reported
// ID resumes: a new session's SessionStart and first prompt wait for that save,
// and a --no-session run reports nothing. Pi reports startup when it opens a
// saved session (pi --session or --continue), so the module reports that
// SessionStart with source resume. A failed command's error ends with the
// tail of its stderr. Uninstall deletes the module once no application's
// hooks remain. A root index.ts, index.js, or package.json pi.extensions in
// Pi's extensions directory stops Pi from loading the module. It needs Pi
// 0.80.4 or later. The OpenCode 2.x TUI plugin reports the root session its
// terminal shows and that session's lifecycle events.
// Several terminals can report the same session_id, so hosts should keep one
// record per terminal.
//
// Applications identify their hooks with a stable marker embedded in the
// command. Reinstalling replaces commands carrying that marker even when the
// executable path changed, and uninstalling removes only those commands.
// Nested JSON reinstalls retain the group and handler positions of existing
// event/matcher registrations. Changed matchers reuse single-handler groups
// in the same event, and new registrations append after existing groups.
// Removing registrations can renumber later hooks.
//
// For example, install an application's Hermes hooks with a stable marker:
//
//	result, err := agenthook.Install(agenthook.AgentHermes, agenthook.InstallOptions{
//		Executable: "/opt/example",
//		Arguments:  []string{"agent-hook", "run", "--source", "example-agent-hook"},
//		Marker:     "--source example-agent-hook",
//		Hooks: []agenthook.Hook{
//			{Event: agenthook.EventPreToolUse, Matcher: agenthook.ToolBash},
//			{Event: agenthook.EventStop},
//		},
//	})
//
// Check inspects an application's registrations for Claude Code or Codex without
// executing hooks or changing hook config or trust. Starting Codex app-server may
// create state files and install bundled skills. Pass the desired InstallOptions
// directly. State is ok, missing, outdated, untrusted, modified, disabled,
// or unknown; Reason and NextStep explain any action needed.
// Executable lookup uses the checking process's environment and PATH.
// Plain command strings use POSIX syntax on every platform. Codex's active
// commandWindows override uses Win32 argument quoting on Windows.
// Missing registrations return missing before inspecting disablement or starting
// Codex.
// Codex checks its current workspace's effective feature, discovery, enablement,
// and trust through app-server. For discovered Codex hooks, states take precedence:
// disabled, modified, outdated, untrusted, ok. Only enabled trusted or managed
// hooks can be ok. Review consent remains with Codex's /hooks interface.
// Claude Code has no native hook probe here: ok covers its active user
// registrations, disableAllHooks in user, project, and local settings for the
// current directory, and executable resolution. Managed policy remains unverified.
// Raw POSIX commands inspect only the executable word; argument execution remains
// unverified. Executable expansions, compound commands, and executable lookup
// failures return unknown with the reason and a corrective step. Neither check
// establishes runtime permissions or successful hook execution. A config outside
// the active user config returns unknown; set CODEX_HOME or CLAUDE_CONFIG_DIR to
// inspect a separate user config.
//
// Installing a Hermes profile does not enable hooks_auto_accept. Hermes retains
// its first-use consent flow for every event and command pair.
//
// A command installed for multiple harnesses can use one typed Claude handler.
// Embed NoopHandler and override only the events the application needs:
//
//	type hooks struct {
//		agenthook.NoopHandler
//	}
//
//	func (hooks) PostToolUse(
//		ctx context.Context,
//		input agenthook.PostToolUseInput,
//	) (agenthook.PostToolUseOutput, error) {
//		return recordToolUse(ctx, input)
//	}
//
//	err := agenthook.Handle(ctx, agent, os.Stdin, os.Stdout, hooks{})
//
// Handle translates native fields before typed dispatch, then translates the
// typed Claude-style output into the invoking harness's response format.
// CommonInput.Raw keeps the complete normalized payload so handlers can inspect
// extension fields.
package agenthook
