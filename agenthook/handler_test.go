package agenthook

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type selectiveHandler struct {
	NoopHandler
	postToolUse *PostToolUseInput
	stop        *StopInput
}

var _ Handler = (*selectiveHandler)(nil)

func (h *selectiveHandler) PostToolUse(
	_ context.Context,
	input PostToolUseInput,
) (Output, error) {
	h.postToolUse = &input
	return Output{
		HookSpecificOutput: &HookSpecificOutput{
			AdditionalContext: "run the focused tests",
		},
	}, nil
}

func (h *selectiveHandler) Stop(
	_ context.Context,
	input StopInput,
) (Output, error) {
	h.stop = &input
	return Output{Decision: DecisionBlock, Reason: "work remains"}, nil
}

func TestHandleNormalizesAndDispatchesTypedPostToolUse(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := &selectiveHandler{}
	var output bytes.Buffer

	err := Handle(
		context.Background(),
		AgentHermes,
		strings.NewReader(`{
  "session_id":"h1",
  "hook_event_name":"post_tool_call",
  "tool_name":"terminal",
  "tool_input":{"command":"go test ./agenthook"},
  "extra":{"result":"ok","tool_call_id":"call-1","turn_id":"turn-1","status":"ok"}
}`),
		&output,
		handler,
	)

	require.NoError(err)
	require.NotNil(handler.postToolUse)
	input := handler.postToolUse
	assert.Equal("h1", input.SessionID)
	assert.Equal(EventPostToolUse, input.HookEventName)
	assert.Equal(ToolBash, input.ToolName)
	assert.Equal("call-1", input.ToolUseID)
	assert.Equal("turn-1", input.TurnID)
	assert.JSONEq(`"ok"`, string(input.ToolResponse))
	assert.JSONEq(`{"command":"go test ./agenthook"}`, string(input.ToolInput))
	assert.Contains(string(input.Raw), `"extra"`)
	assert.JSONEq(`{
  "hookSpecificOutput": {
    "hookEventName": "PostToolUse",
    "additionalContext": "run the focused tests"
  }
}`, output.String())
}

func TestHandleDispatchesTypedStop(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	handler := &selectiveHandler{}
	var output bytes.Buffer

	err := Handle(
		context.Background(),
		AgentGemini,
		strings.NewReader(`{
  "session_id":"g1",
  "hook_event_name":"AfterAgent",
  "prompt_response":"finished",
  "stop_hook_active":true
}`),
		&output,
		handler,
	)

	require.NoError(err)
	require.NotNil(handler.stop)
	assert.Equal("g1", handler.stop.SessionID)
	assert.Equal(EventStop, handler.stop.HookEventName)
	assert.True(handler.stop.StopHookActive)
	assert.Equal("finished", handler.stop.LastAssistantMessage)
	assert.JSONEq(`{"decision":"block","reason":"work remains"}`, output.String())
}

func TestNoopHandlerImplementsEveryTypedEvent(t *testing.T) {
	var handler Handler = NoopHandler{}
	var output bytes.Buffer

	err := Handle(
		context.Background(),
		AgentClaude,
		strings.NewReader(`{"session_id":"c1","hook_event_name":"Notification","message":"ready"}`),
		&output,
		handler,
	)

	require.NoError(t, err)
	assert.JSONEq(t, `{}`, output.String())
}

func TestHandleRejectsUnknownEventsBeforeCallingHandler(t *testing.T) {
	var output bytes.Buffer

	err := Handle(
		context.Background(),
		AgentClaude,
		strings.NewReader(`{"session_id":"c1","hook_event_name":"FutureEvent"}`),
		&output,
		NoopHandler{},
	)

	require.Error(t, err)
	assert.ErrorContains(t, err, `unsupported Claude hook event "FutureEvent"`)
	assert.Empty(t, output.String())
}

func TestOutputMarshalsTypedPermissionDecision(t *testing.T) {
	output := Output{
		HookSpecificOutput: &HookSpecificOutput{
			HookEventName:            EventPreToolUse,
			PermissionDecision:       PermissionDecisionDeny,
			PermissionDecisionReason: "production command",
			UpdatedInput:             json.RawMessage(`{"command":"true"}`),
		},
	}

	data, err := json.Marshal(output)

	require.NoError(t, err)
	assert.JSONEq(t, `{
  "hookSpecificOutput": {
    "hookEventName": "PreToolUse",
    "permissionDecision": "deny",
    "permissionDecisionReason": "production command",
    "updatedInput": {"command":"true"}
  }
}`, string(data))
}
