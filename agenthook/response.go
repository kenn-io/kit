package agenthook

import (
	"encoding/json"
	"errors"
	"fmt"
)

func encodeClaudeResponse(event Event, value any) (map[string]any, error) {
	response := map[string]any{}
	specific := map[string]any{"hookEventName": event}
	addSpecific := false
	switch output := value.(type) {
	case SessionStartOutput:
		addCommonOutput(response, output.CommonOutput)
		addSpecific = addValue(specific, "additionalContext", output.AdditionalContext) || addSpecific
		addSpecific = addValue(specific, "initialUserMessage", output.InitialUserMessage) || addSpecific
		addSpecific = addValue(specific, "sessionTitle", output.SessionTitle) || addSpecific
		if len(output.WatchPaths) > 0 {
			specific["watchPaths"] = output.WatchPaths
			addSpecific = true
		}
		if output.ReloadSkills {
			specific["reloadSkills"] = true
			addSpecific = true
		}
	case UserPromptSubmitOutput:
		addCommonOutput(response, output.CommonOutput)
		addDecision(response, output.Decision, output.Reason)
		addSpecific = addValue(specific, "additionalContext", output.AdditionalContext) || addSpecific
		addSpecific = addValue(specific, "sessionTitle", output.SessionTitle) || addSpecific
		if output.SuppressOriginalPrompt {
			specific["suppressOriginalPrompt"] = true
			addSpecific = true
		}
	case PreToolUseOutput:
		addCommonOutput(response, output.CommonOutput)
		addSpecific = addValue(specific, "permissionDecision", output.PermissionDecision) || addSpecific
		addSpecific = addValue(specific, "permissionDecisionReason", output.PermissionDecisionReason) || addSpecific
		addSpecific = addRaw(specific, "updatedInput", output.UpdatedInput) || addSpecific
		addSpecific = addValue(specific, "additionalContext", output.AdditionalContext) || addSpecific
	case PostToolUseOutput:
		addCommonOutput(response, output.CommonOutput)
		addDecision(response, output.Decision, output.Reason)
		addSpecific = addValue(specific, "additionalContext", output.AdditionalContext) || addSpecific
		addSpecific = addRaw(specific, "updatedToolOutput", output.UpdatedToolOutput) || addSpecific
		addSpecific = addRaw(specific, "updatedMCPToolOutput", output.UpdatedMCPToolOutput) || addSpecific
	case PostToolUseFailureOutput:
		addCommonOutput(response, output.CommonOutput)
		addSpecific = addValue(specific, "additionalContext", output.AdditionalContext)
	case PermissionRequestOutput:
		addCommonOutput(response, output.CommonOutput)
		if output.Decision != nil {
			specific["decision"] = output.Decision
			addSpecific = true
		}
	case NotificationOutput:
		addCommonOutput(response, output.CommonOutput)
	case StopOutput:
		addCommonOutput(response, output.CommonOutput)
		addDecision(response, output.Decision, output.Reason)
		addSpecific = addValue(specific, "additionalContext", output.AdditionalContext)
	case SessionEndOutput:
		addCommonOutput(response, output.CommonOutput)
	default:
		return nil, fmt.Errorf("unexpected typed output %T", value)
	}
	if addSpecific {
		response["hookSpecificOutput"] = specific
	}
	return response, nil
}

func encodeCopilotResponse(event Event, value any) (map[string]any, error) {
	response := map[string]any{}
	switch output := value.(type) {
	case SessionStartOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.InitialUserMessage != "" || output.SessionTitle != "" ||
			len(output.WatchPaths) > 0 || output.ReloadSkills {
			return nil, errors.New("unsupported SessionStart output fields")
		}
		addValue(response, "additionalContext", output.AdditionalContext)
	case UserPromptSubmitOutput:
		if !isZeroOutput(output) {
			return nil, errors.New("UserPromptSubmit output is observational")
		}
	case PreToolUseOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.AdditionalContext != "" || output.PermissionDecision == PermissionDecisionDefer {
			return nil, errors.New("unsupported PreToolUse output fields")
		}
		addValue(response, "permissionDecision", output.PermissionDecision)
		addValue(response, "permissionDecisionReason", output.PermissionDecisionReason)
		addRaw(response, "modifiedArgs", output.UpdatedInput)
	case PostToolUseOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.Decision != "" || output.Reason != "" || len(output.UpdatedMCPToolOutput) > 0 {
			return nil, errors.New("unsupported PostToolUse output fields")
		}
		addValue(response, "additionalContext", output.AdditionalContext)
		addRaw(response, "modifiedResult", output.UpdatedToolOutput)
	case PostToolUseFailureOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		addValue(response, "additionalContext", output.AdditionalContext)
	case PermissionRequestOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.Decision != nil {
			if len(output.Decision.UpdatedInput) > 0 || len(output.Decision.UpdatedPermissions) > 0 {
				return nil, errors.New("unsupported PermissionRequest output fields")
			}
			response["behavior"] = output.Decision.Behavior
			addValue(response, "message", output.Decision.Message)
			if output.Decision.Interrupt {
				response["interrupt"] = true
			}
		}
	case StopOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.AdditionalContext != "" {
			return nil, errors.New("unsupported Stop output fields")
		}
		addDecision(response, output.Decision, output.Reason)
	case NotificationOutput:
		if !isZeroOutput(output) {
			return nil, errors.New("unsupported Notification output fields")
		}
	case SessionEndOutput:
		if !isZeroOutput(output) {
			return nil, errors.New("unsupported SessionEnd output fields")
		}
	default:
		return nil, fmt.Errorf("unexpected typed output %T", value)
	}
	return response, nil
}

// encodeGeminiResponse follows Gemini CLI's native hook response contract.
// BeforeTool decisions are top-level, while input rewrites use
// hookSpecificOutput.tool_input:
// https://github.com/google-gemini/gemini-cli/blob/main/docs/hooks/reference.md#beforetool
// https://github.com/google-gemini/gemini-cli/blob/main/packages/core/src/hooks/types.ts#L327-L347
func encodeGeminiResponse(event Event, value any) (map[string]any, error) {
	response := map[string]any{}
	addSpecific := func(fields map[string]any) {
		if len(fields) == 0 {
			return
		}
		fields["hookEventName"] = geminiEventName(event)
		response["hookSpecificOutput"] = fields
	}

	switch output := value.(type) {
	case SessionStartOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
		if output.InitialUserMessage != "" || output.SessionTitle != "" ||
			len(output.WatchPaths) > 0 || output.ReloadSkills {
			return nil, errors.New("unsupported SessionStart output fields")
		}
		specific := map[string]any{}
		addValue(specific, "additionalContext", output.AdditionalContext)
		addSpecific(specific)
	case UserPromptSubmitOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
		if output.SessionTitle != "" || output.SuppressOriginalPrompt {
			return nil, errors.New("unsupported UserPromptSubmit output fields")
		}
		addDecision(response, output.Decision, output.Reason)
		specific := map[string]any{}
		addValue(specific, "additionalContext", output.AdditionalContext)
		addSpecific(specific)
	case PreToolUseOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
		if output.PermissionDecision == PermissionDecisionDefer || output.AdditionalContext != "" {
			return nil, errors.New("unsupported PreToolUse output fields")
		}
		addValue(response, "decision", output.PermissionDecision)
		addValue(response, "reason", output.PermissionDecisionReason)
		specific := map[string]any{}
		addRaw(specific, "tool_input", output.UpdatedInput)
		addSpecific(specific)
	case PostToolUseOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
		if len(output.UpdatedToolOutput) > 0 || len(output.UpdatedMCPToolOutput) > 0 {
			return nil, errors.New("unsupported PostToolUse output fields")
		}
		addDecision(response, output.Decision, output.Reason)
		specific := map[string]any{}
		addValue(specific, "additionalContext", output.AdditionalContext)
		addSpecific(specific)
	case NotificationOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
	case StopOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
		if output.AdditionalContext != "" {
			return nil, errors.New("unsupported Stop output fields")
		}
		addDecision(response, output.Decision, output.Reason)
	case SessionEndOutput:
		if err := addGeminiCommonOutput(response, output.CommonOutput); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("Gemini CLI does not support %s output", event)
	}
	return response, nil
}

func addGeminiCommonOutput(response map[string]any, output CommonOutput) error {
	if output.TerminalSequence != "" {
		return errors.New("terminalSequence is unsupported")
	}
	addCommonOutput(response, output)
	return nil
}

func encodeHermesResponse(event Event, value any) (map[string]any, error) {
	response := map[string]any{}
	if isZeroOutput(value) {
		return response, nil
	}
	switch output := value.(type) {
	case UserPromptSubmitOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.Decision != "" || output.Reason != "" || output.SessionTitle != "" ||
			output.SuppressOriginalPrompt {
			return nil, errors.New("unsupported UserPromptSubmit output fields")
		}
		addValue(response, "context", output.AdditionalContext)
	case PreToolUseOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		if output.PermissionDecision != PermissionDecisionDeny ||
			len(output.UpdatedInput) > 0 || output.AdditionalContext != "" {
			return nil, errors.New("unsupported PreToolUse output fields")
		}
		response["decision"] = DecisionBlock
		response["reason"] = output.PermissionDecisionReason
	case StopOutput:
		if err := requireEmptyCommon(output.CommonOutput); err != nil {
			return nil, err
		}
		reason := output.Reason
		if reason == "" {
			reason = output.AdditionalContext
		}
		if output.Decision != DecisionBlock && output.AdditionalContext == "" {
			return nil, errors.New("unsupported Stop output fields")
		}
		response["decision"] = DecisionBlock
		response["reason"] = reason
	default:
		return nil, fmt.Errorf("Hermes does not support %s control output", event)
	}
	return response, nil
}

func addCommonOutput(response map[string]any, output CommonOutput) {
	if output.Continue != nil {
		response["continue"] = *output.Continue
	}
	addValue(response, "stopReason", output.StopReason)
	if output.SuppressOutput {
		response["suppressOutput"] = true
	}
	addValue(response, "systemMessage", output.SystemMessage)
	addValue(response, "terminalSequence", output.TerminalSequence)
}

func addDecision(response map[string]any, decision Decision, reason string) {
	addValue(response, "decision", decision)
	addValue(response, "reason", reason)
}

func addValue[T ~string](response map[string]any, key string, value T) bool {
	if value == "" {
		return false
	}
	response[key] = value
	return true
}

func addRaw(response map[string]any, key string, value json.RawMessage) bool {
	if len(value) == 0 {
		return false
	}
	response[key] = value
	return true
}

func requireEmptyCommon(output CommonOutput) error {
	if !isZeroOutput(output) {
		return errors.New("common Claude output fields are unsupported")
	}
	return nil
}
