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
// A command installed for multiple harnesses can keep one Claude-shaped
// handler. Pass the selected agent to Normalize before decoding or forwarding
// the hook payload:
//
//	payload, err := agenthook.Normalize(agent, os.Stdin)
//	if err != nil {
//		return err
//	}
//	return handleClaudeHook(bytes.NewReader(payload))
//
// Normalize retains native fields that have no Claude equivalent. Consumers
// can therefore use a Claude-compatible struct while still inspecting
// harness-specific extensions when needed.
package agenthook
