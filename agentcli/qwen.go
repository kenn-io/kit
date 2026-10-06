package agentcli

// NewQwen returns an adapter with the configured executable and arguments.
// Qwen Code is a Gemini CLI fork that keeps Gemini's flags.
func NewQwen(command Command) (Adapter, error) {
	return newAdapter(Qwen, command, "qwen", geminiCapabilities, buildGemini)
}
