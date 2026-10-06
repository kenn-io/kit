// Package agenthook installs command hooks into supported agent harnesses and
// normalizes their input for Claude Code-style hook handlers.
//
// Its public event and matcher vocabulary follows Claude Code. Agent profiles
// translate that vocabulary to the native config path, event names, tool names,
// and file format used by each harness. This lets applications describe one set
// of lifecycle hooks while support for new agents stays centralized in kit.
// Profiles are provided for Claude Code, Codex, GitHub Copilot CLI, Cursor,
// Factory Droid, Gemini CLI, Hermes Agent, OpenCode, Pi, and Qwen Code. Pi and
// OpenCode have no command-hook config, so their profiles write a kit-owned
// module that runs the registered commands. The Pi module reports only from
// Pi's interactive terminal, so print, json, and rpc runs, subagents included,
// stay silent. Its SessionEnd fires only when another session replaces the
// current one (new, resume, or fork), never on quit or reload. The module
// reports an event only once Pi has saved the session file, so every reported
// ID resumes: a new session's SessionStart and first prompt wait for that save,
// and a --no-session run reports nothing. A root index.ts, index.js, or
// package.json pi.extensions in Pi's extensions directory stops Pi from loading
// the module. It needs Pi 0.80.4 or later. The OpenCode module is a 2.x TUI
// plugin that reports the root session its terminal shows, and ends that
// session's report when the terminal moves to another root. Switching session
// tabs counts as such a move, so a background tab's finished turn sends no
// Stop. It also ends the report when that root is deleted, re-sends
// SessionStart after a reload unless a turn is running, and reports the
// directory the terminal launched OpenCode in as cwd. Opening a root whose turn
// is already running reports it idle until that turn's Stop. Several terminals
// can show one OpenCode root, so key terminal state by the terminal's runtime
// key rather than the session ID.
//
// Applications identify their hooks with a stable marker embedded in the
// command. Reinstalling replaces commands carrying that marker even when the
// executable path changed, and uninstalling removes only those commands:
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
