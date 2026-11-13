package agentmcp

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func planDocument(agent Agent, original []byte, name string, entry map[string]any, optionalArgs []string, checkOnly bool) ([]byte, bool, bool, error) {
	doc := map[string]any{}
	if len(original) > 0 {
		if agent == AgentCodex {
			if _, err := toml.Decode(string(original), &doc); err != nil {
				return nil, false, false, err
			}
		} else {
			decoder := yaml.NewDecoder(bytes.NewReader(original))
			if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
				doc = map[string]any{}
			} else if err != nil {
				return nil, false, false, err
			}
			var extra yaml.Node
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return nil, false, false, errors.New("MCP config must contain one YAML document")
			}
		}
		if doc == nil {
			return nil, false, false, errors.New("MCP config must be a mapping")
		}
	}
	servers := map[string]any{}
	if value, ok := doc["mcp_servers"]; ok {
		var valid bool
		servers, valid = value.(map[string]any)
		if !valid {
			return nil, false, false, errors.New("mcp_servers must be a mapping")
		}
	}
	previous, present := servers[name]
	if present {
		if fields, ok := previous.(map[string]any); ok {
			fields = maps.Clone(fields)
			dropEmptyFields(fields)
			if agent == AgentCodex {
				entry = mergeEntry(agent, fields, entry, optionalArgs)
			}
			previous = fields
		}
		before, err := encodeDocument(agent, previous)
		if err != nil {
			return nil, false, false, err
		}
		after, err := encodeDocument(agent, entry)
		if err != nil {
			return nil, false, false, err
		}
		if bytes.Equal(before, after) {
			return original, false, true, nil
		}
	}
	if checkOnly {
		return nil, true, present, nil
	}
	var data []byte
	var err error
	if agent == AgentHermes {
		data, err = editYAML(original, name, entry, servers)
	} else {
		data, err = editTOML(original, name, entry)
	}
	return data, true, present, err
}

// mergeEntry keeps user settings for a launch and env update on the same transport.
func mergeEntry(agent Agent, previous, desired map[string]any, optionalArgs []string) map[string]any {
	launch := []string{"command", "args", "url", "type"}
	for key := range desired {
		if key != "env" && !slices.Contains(launch, key) {
			return desired
		}
	}
	_, oldURL := previous["url"]
	_, newURL := desired["url"]
	oldType, _ := previous["type"].(string)
	newType, _ := desired["type"].(string)
	if oldURL != newURL || (agent == AgentClaude && oldURL && oldType != newType) {
		return desired
	}
	merged := maps.Clone(previous)
	for _, key := range launch {
		delete(merged, key)
	}
	maps.Copy(merged, desired)
	if supplied, ok := desired["env"].(map[string]string); ok {
		env, _ := previous["env"].(map[string]any)
		env = maps.Clone(env)
		if env == nil {
			env = make(map[string]any)
		}
		for key, value := range supplied {
			env[key] = value
		}
		merged["env"] = env
	}
	if len(optionalArgs) > 0 {
		args, _ := desired["args"].([]string)
		carried := make(map[string]bool, len(args))
		for _, arg := range args {
			name, _, _ := strings.Cut(arg, "=")
			carried[name] = true
		}
		var required, saved []string
		old, ok := previous["args"].([]any)
		for _, value := range old {
			arg, valid := value.(string)
			if !valid {
				ok = false
				break
			}
			name, _, _ := strings.Cut(arg, "=")
			if slices.Contains(optionalArgs, arg) && !carried[name] {
				saved = append(saved, arg)
			} else {
				required = append(required, arg)
			}
		}
		if ok && slices.Equal(required, args) {
			merged["args"] = old
		} else if combined := append(slices.Clone(args), saved...); len(combined) > 0 {
			merged["args"] = combined
		}
	}
	return merged
}

// dropEmptyFields removes the empty args and env written by agent add commands.
func dropEmptyFields(fields map[string]any) {
	if args, ok := fields["args"].([]any); ok && len(args) == 0 {
		delete(fields, "args")
	}
	if env, ok := fields["env"].(map[string]any); ok && len(env) == 0 {
		delete(fields, "env")
	}
}

func encodeDocument(agent Agent, value any) ([]byte, error) {
	if agent == AgentHermes {
		return yaml.Marshal(value)
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
