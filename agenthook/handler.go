package agenthook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Handler receives normalized, Claude-shaped hook events. Embed NoopHandler
// and override only the event methods an application needs.
type Handler interface {
	SessionStart(context.Context, SessionStartInput) (Output, error)
	UserPromptSubmit(context.Context, UserPromptSubmitInput) (Output, error)
	PreToolUse(context.Context, PreToolUseInput) (Output, error)
	PostToolUse(context.Context, PostToolUseInput) (Output, error)
	PostToolUseFailure(context.Context, PostToolUseFailureInput) (Output, error)
	PermissionRequest(context.Context, PermissionRequestInput) (Output, error)
	Notification(context.Context, NotificationInput) (Output, error)
	Stop(context.Context, StopInput) (Output, error)
	SessionEnd(context.Context, SessionEndInput) (Output, error)
}

// NoopHandler implements Handler without taking any action. Applications can
// embed it and override only the event methods they handle.
type NoopHandler struct{}

func (NoopHandler) SessionStart(context.Context, SessionStartInput) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) UserPromptSubmit(context.Context, UserPromptSubmitInput) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) PreToolUse(context.Context, PreToolUseInput) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) PostToolUse(context.Context, PostToolUseInput) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) PostToolUseFailure(
	context.Context,
	PostToolUseFailureInput,
) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) PermissionRequest(
	context.Context,
	PermissionRequestInput,
) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) Notification(context.Context, NotificationInput) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) Stop(context.Context, StopInput) (Output, error) {
	return Output{}, nil
}

func (NoopHandler) SessionEnd(context.Context, SessionEndInput) (Output, error) {
	return Output{}, nil
}

// Handle normalizes one native agent payload, dispatches its Claude event to
// handler, and writes the typed Claude hook output as JSON.
func Handle(
	ctx context.Context,
	agent Agent,
	input io.Reader,
	output io.Writer,
	handler Handler,
) error {
	if handler == nil {
		return errors.New("agent hook handler is required")
	}
	if output == nil {
		return errors.New("agent hook output writer is required")
	}
	payload, err := normalize(agent, input)
	if err != nil {
		return err
	}

	var envelope struct {
		HookEventName Event `json:"hook_event_name"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode normalized agent hook payload: %w", err)
	}
	if envelope.HookEventName == "" {
		return errors.New("normalized agent hook payload missing hook_event_name")
	}

	result, err := dispatch(ctx, handler, envelope.HookEventName, payload)
	if err != nil {
		return fmt.Errorf("handle %s agent hook: %w", envelope.HookEventName, err)
	}
	if result.HookSpecificOutput != nil {
		specific := result.HookSpecificOutput
		if specific.HookEventName == "" {
			specific.HookEventName = envelope.HookEventName
		} else if specific.HookEventName != envelope.HookEventName {
			return fmt.Errorf(
				"handle %s agent hook: hook-specific output names %s",
				envelope.HookEventName, specific.HookEventName,
			)
		}
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return fmt.Errorf("encode %s agent hook output: %w", envelope.HookEventName, err)
	}
	return nil
}

func dispatch(
	ctx context.Context,
	handler Handler,
	event Event,
	payload []byte,
) (Output, error) {
	switch event {
	case EventSessionStart:
		var input SessionStartInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.SessionStart(ctx, input)
	case EventUserPromptSubmit:
		var input UserPromptSubmitInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.UserPromptSubmit(ctx, input)
	case EventPreToolUse:
		var input PreToolUseInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.PreToolUse(ctx, input)
	case EventPostToolUse:
		var input PostToolUseInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.PostToolUse(ctx, input)
	case EventPostToolUseFailure:
		var input PostToolUseFailureInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.PostToolUseFailure(ctx, input)
	case EventPermissionRequest:
		var input PermissionRequestInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.PermissionRequest(ctx, input)
	case EventNotification:
		var input NotificationInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.Notification(ctx, input)
	case EventStop:
		var input StopInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.Stop(ctx, input)
	case EventSessionEnd:
		var input SessionEndInput
		if err := decodeTypedInput(payload, &input.CommonInput, &input); err != nil {
			return Output{}, err
		}
		return handler.SessionEnd(ctx, input)
	default:
		return Output{}, fmt.Errorf("unsupported Claude hook event %q", event)
	}
}

func decodeTypedInput(payload []byte, common *CommonInput, target any) error {
	if err := json.Unmarshal(payload, target); err != nil {
		return fmt.Errorf("decode typed agent hook input: %w", err)
	}
	common.Raw = append(common.Raw[:0], payload...)
	return nil
}
