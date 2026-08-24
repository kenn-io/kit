package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func planOpenCodePlugin(
	path, marker, command, commandWindows string,
	hooks []nativeHook,
	uninstall bool,
) ([]byte, bool, error) {
	before, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("read OpenCode agent hook plugin %s: %w", path, err)
	}
	if exists && !bytes.Contains(before, []byte(marker)) {
		return nil, false, fmt.Errorf("OpenCode plugin %s is not owned by marker %q", path, marker)
	}
	if uninstall {
		return nil, exists, nil
	}
	after := renderOpenCodePlugin(command, commandWindows, hooks)
	return after, !bytes.Equal(before, after), nil
}

func renderOpenCodePlugin(command, commandWindows string, hooks []nativeHook) []byte {
	if commandWindows == "" {
		commandWindows = command
	}
	timeout := 0
	for _, hook := range hooks {
		if hook.timeout > timeout {
			timeout = hook.timeout
		}
	}
	if timeout == 0 {
		timeout = 10
	}

	var body strings.Builder
	fmt.Fprintf(&body, `import { spawn } from "node:child_process"

const command = process.platform === "win32" ? %s : %s
const timeout = %d

const invoke = (cwd, payload) => new Promise((resolve) => {
  let settled = false
  const finish = (value) => {
    if (settled) return
    settled = true
    clearTimeout(timer)
    resolve(value)
  }
  const child = spawn(command, { cwd, shell: true, windowsHide: true })
  let stdout = ""
  let stderr = ""
  child.stdout.on("data", (chunk) => { stdout += chunk })
  child.stderr.on("data", (chunk) => { stderr += chunk })
  child.on("error", (error) => {
    console.error("OpenCode agent hook:", error.message)
    finish({})
  })
  child.on("close", (code) => {
    if (code !== 0) {
      if (stderr.trim()) console.error("OpenCode agent hook:", stderr.trim())
      finish({})
      return
    }
    try {
      finish(JSON.parse(stdout))
    } catch (error) {
      console.error("OpenCode agent hook: invalid JSON response:", error.message)
      finish({})
    }
  })
  const timer = setTimeout(() => {
    child.kill()
    console.error("OpenCode agent hook: command timed out")
    finish({})
  }, timeout)
  child.stdin.end(JSON.stringify(payload))
})

const addContext = (text, context) => context ? text + "\n\n" + context : text

export const AgentHookPlugin = async ({ directory, worktree }) => ({
`, strconv.Quote(commandWindows), strconv.Quote(command), timeout*1000)

	for _, event := range []Event{EventUserPromptSubmit, EventPreToolUse, EventPostToolUse} {
		matching := hooksForEvent(hooks, event)
		if len(matching) == 0 {
			continue
		}
		switch event {
		case EventUserPromptSubmit:
			body.WriteString(`  "chat.message": async (input, output) => {
    const response = await invoke(worktree || directory, {
      session_id: input.sessionID,
      turn_id: input.messageID,
      cwd: directory,
      worktree,
      hook_event_name: "chat.message",
      prompt: output.parts.filter((part) => part.type === "text").map((part) => part.text).join("\n"),
    })
    if (!response.additionalContext) return
    const part = [...output.parts].reverse().find((item) => item.type === "text")
    if (part) part.text = addContext(part.text, response.additionalContext)
  },
`)
		case EventPreToolUse:
			body.WriteString(`  "tool.execute.before": async (input, output) => {
`)
			writeOpenCodeToolInvocations(&body, matching, false)
			body.WriteString("  },\n")
		case EventPostToolUse:
			body.WriteString(`  "tool.execute.after": async (input, output) => {
`)
			writeOpenCodeToolInvocations(&body, matching, true)
			body.WriteString("  },\n")
		}
	}
	body.WriteString("})\n")
	return []byte(body.String())
}

func hooksForEvent(hooks []nativeHook, event Event) []nativeHook {
	result := make([]nativeHook, 0, len(hooks))
	for _, hook := range hooks {
		if hook.event == event {
			result = append(result, hook)
		}
	}
	return result
}

func writeOpenCodeToolInvocations(body *strings.Builder, hooks []nativeHook, after bool) {
	for _, hook := range hooks {
		if hook.matcher != "" {
			fmt.Fprintf(body, "    if (input.tool === %s) {\n", strconv.Quote(hook.matcher))
		}
		indent := "    "
		if hook.matcher != "" {
			indent = "      "
		}
		response := ""
		if after {
			response = "const response = "
		}
		fmt.Fprintf(body, `%s%sawait invoke(worktree || directory, {
%s  session_id: input.sessionID,
%s  cwd: directory,
%s  worktree,
%s  hook_event_name: %s,
%s  tool_name: input.tool,
%s  tool_use_id: input.callID,
%s  tool_input: %s,
`, indent, response, indent, indent, indent, indent, strconv.Quote(hook.name), indent, indent, indent, toolInputExpression(after))
		if after {
			fmt.Fprintf(body, "%s  tool_response: output,\n", indent)
		}
		fmt.Fprintf(body, "%s})\n", indent)
		if after {
			fmt.Fprintf(body, "%soutput.output = addContext(output.output, response.additionalContext)\n", indent)
		}
		if hook.matcher != "" {
			body.WriteString("    }\n")
		}
	}
}

func toolInputExpression(after bool) string {
	if after {
		return "input.args"
	}
	return "output.args"
}
