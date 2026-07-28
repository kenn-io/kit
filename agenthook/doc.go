// Package agenthook installs command hooks into supported agent harnesses and
// normalizes their input for Claude Code-style hook handlers.
//
// Its public event and matcher vocabulary follows Claude Code. Agent profiles
// translate that vocabulary to the native config path, event names, tool names,
// and file format used by each harness. This lets applications describe one set
// of lifecycle hooks while support for new agents stays centralized in kit.
// Profiles are provided for Claude Code, Codex, GitHub Copilot CLI, Cursor,
// Factory Droid, Gemini CLI, Hermes Agent, and Qwen Code.
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
