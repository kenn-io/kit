package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
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
	spec := newProfileSpec(
		Profile{
			Agent: AgentHermes, DisplayName: "Hermes Agent",
			ConfigEnvironment: "HERMES_HOME", ConfigFilename: "config.yaml",
			SupportedEvents: hermesSupportedEvents,
		},
		formatHermesYAML,
		"terminal",
		hermesDefaultDir,
	)
	spec.eventName = hermesEventName
	return spec
}

func hermesEventName(event Event) string {
	field, ok := reflect.TypeFor[hermesHooks]().FieldByName(string(event))
	if !ok {
		return string(event)
	}
	name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
	if name == "" {
		return string(event)
	}
	return name
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
		if err := removeOwnedHermesHookNodes(hooksNode, marker, path); err != nil {
			return nil, false, err
		}
		if !uninstall {
			for _, hook := range hooks {
				if err := appendHermesHookNode(hooksNode, hook.name, hermesHook{
					Matcher: hook.matcher,
					Command: command,
					Timeout: hook.timeout,
				}, path); err != nil {
					return nil, false, err
				}
			}
		}
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

func removeOwnedHermesHookNodes(hooks *yaml.Node, marker, path string) error {
	for i := 0; i+1 < len(hooks.Content); {
		event := hooks.Content[i]
		entries := hooks.Content[i+1]
		if entries.Kind != yaml.SequenceNode {
			if isHermesEventName(event.Value) {
				return fmt.Errorf(
					"Hermes config %s event %q must be an array", path, event.Value,
				)
			}
			i += 2
			continue
		}
		kept := make([]*yaml.Node, 0, len(entries.Content))
		for _, entry := range entries.Content {
			command := yamlField(entry, "command")
			if command != nil && command.Kind == yaml.ScalarNode &&
				strings.Contains(command.Value, marker) {
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == 0 && len(entries.Content) > 0 {
			hooks.Content = append(hooks.Content[:i], hooks.Content[i+2:]...)
			continue
		}
		entries.Content = kept
		i += 2
	}
	return nil
}

func appendHermesHookNode(hooks *yaml.Node, event string, hook hermesHook, path string) error {
	entries := yamlField(hooks, event)
	if entries == nil {
		entries = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		hooks.Content = append(hooks.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: event},
			entries,
		)
	}
	if entries.Kind != yaml.SequenceNode {
		return fmt.Errorf("Hermes config %s event %q must be an array", path, event)
	}
	var encoded yaml.Node
	if err := encoded.Encode(hook); err != nil {
		return fmt.Errorf("encode Hermes hook %q in %s: %w", event, path, err)
	}
	entries.Content = append(entries.Content, &encoded)
	return nil
}

func isHermesEventName(name string) bool {
	for _, event := range hermesSupportedEvents {
		if hermesEventName(event) == name {
			return true
		}
	}
	return false
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
