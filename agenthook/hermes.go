package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type hermesHook struct {
	Matcher string               `yaml:"matcher,omitempty"`
	Command string               `yaml:"command"`
	Timeout int                  `yaml:"timeout,omitempty"`
	Extra   map[string]yaml.Node `yaml:",inline"`
}

// hermesHooks uses Claude-style field names as the package-facing vocabulary
// and YAML tags as the single source of truth for Hermes' native event names.
type hermesHooks struct {
	SessionStart      []hermesHook            `yaml:"on_session_start,omitempty"`
	UserPromptSubmit  []hermesHook            `yaml:"pre_llm_call,omitempty"`
	PreToolUse        []hermesHook            `yaml:"pre_tool_call,omitempty"`
	PostToolUse       []hermesHook            `yaml:"post_tool_call,omitempty"`
	PermissionRequest []hermesHook            `yaml:"pre_approval_request,omitempty"`
	Stop              []hermesHook            `yaml:"post_llm_call,omitempty"`
	SessionEnd        []hermesHook            `yaml:"on_session_end,omitempty"`
	Extra             map[string][]hermesHook `yaml:",inline"`
}

var hermesSupportedEvents = []Event{
	EventSessionStart,
	EventUserPromptSubmit,
	EventPreToolUse,
	EventPostToolUse,
	EventPermissionRequest,
	EventStop,
	EventSessionEnd,
}

func hermesProfile() profileSpec {
	return newProfileSpec(
		Profile{
			Agent: AgentHermes, DisplayName: "Hermes Agent",
			ConfigEnvironment: "HERMES_HOME", ConfigFilename: "config.yaml",
			SupportedEvents: hermesSupportedEvents,
		},
		formatHermesYAML,
		"terminal",
		hermesDefaultDir,
	)
}

func planHermesConfig(
	path, marker, command string,
	hooks []nativeHook,
	uninstall bool,
) ([]byte, bool, error) {
	document, exists, err := readHermesConfig(path)
	if err != nil {
		return nil, false, err
	}
	before, err := yaml.Marshal(document)
	if err != nil {
		return nil, false, fmt.Errorf("encode existing Hermes config %s: %w", path, err)
	}
	root := document.Content[0]
	hooksNode, err := yamlMappingField(root, "hooks", path, !uninstall)
	if err != nil {
		return nil, false, err
	}
	if hooksNode != nil {
		var configured hermesHooks
		if err := hooksNode.Decode(&configured); err != nil {
			return nil, false, fmt.Errorf("decode Hermes hooks in %s: %w", path, err)
		}
		configured.removeOwned(marker)
		if !uninstall {
			for _, hook := range hooks {
				entries, ok := configured.entries(hook.event)
				if !ok {
					return nil, false, fmt.Errorf("Hermes does not support %s hooks", hook.event)
				}
				*entries = append(*entries, hermesHook{
					Matcher: hook.matcher,
					Command: command,
					Timeout: hook.timeout,
				})
			}
		}
		var encoded yaml.Node
		if err := encoded.Encode(configured); err != nil {
			return nil, false, fmt.Errorf("encode Hermes hooks in %s: %w", path, err)
		}
		encoded.HeadComment = hooksNode.HeadComment
		encoded.LineComment = hooksNode.LineComment
		encoded.FootComment = hooksNode.FootComment
		*hooksNode = encoded
	}
	after, err := yaml.Marshal(document)
	if err != nil {
		return nil, false, fmt.Errorf("encode Hermes config %s: %w", path, err)
	}
	changed := !bytes.Equal(before, after)
	if uninstall && !exists {
		changed = false
	}
	return after, changed, nil
}

func (h *hermesHooks) entries(event Event) (*[]hermesHook, bool) {
	switch event {
	case EventSessionStart:
		return &h.SessionStart, true
	case EventUserPromptSubmit:
		return &h.UserPromptSubmit, true
	case EventPreToolUse:
		return &h.PreToolUse, true
	case EventPostToolUse:
		return &h.PostToolUse, true
	case EventPermissionRequest:
		return &h.PermissionRequest, true
	case EventStop:
		return &h.Stop, true
	case EventSessionEnd:
		return &h.SessionEnd, true
	default:
		return nil, false
	}
}

func (h *hermesHooks) removeOwned(marker string) {
	for _, event := range hermesSupportedEvents {
		entries, _ := h.entries(event)
		*entries = keepOtherHermesHooks(*entries, marker)
	}
	for event, entries := range h.Extra {
		entries = keepOtherHermesHooks(entries, marker)
		if len(entries) == 0 {
			delete(h.Extra, event)
		} else {
			h.Extra[event] = entries
		}
	}
}

func keepOtherHermesHooks(hooks []hermesHook, marker string) []hermesHook {
	kept := make([]hermesHook, 0, len(hooks))
	for _, hook := range hooks {
		if !strings.Contains(hook.Command, marker) {
			kept = append(kept, hook)
		}
	}
	return kept
}

func readHermesConfig(path string) (*yaml.Node, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyYAMLDocument(), false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Hermes config %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return emptyYAMLDocument(), true, nil
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, false, fmt.Errorf("decode Hermes config %s: %w", path, err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple YAML documents")
		}
		return nil, false, fmt.Errorf("decode Hermes config %s: %w", path, err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 ||
		document.Content[0].Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("Hermes config %s must contain one YAML object", path)
	}
	return &document, true, nil
}

func emptyYAMLDocument() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.DocumentNode,
		Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}},
	}
}

func yamlMappingField(root *yaml.Node, key, path string, create bool) (*yaml.Node, error) {
	value := yamlField(root, key)
	if value == nil {
		if !create {
			return nil, nil
		}
		value = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value,
		)
		return value, nil
	}
	if value.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("Hermes config %s field %q must be an object", path, key)
	}
	return value, nil
}

func yamlField(root *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return root.Content[i+1]
		}
	}
	return nil
}
