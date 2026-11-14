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

func planDocument(agent Agent, original []byte, name string, entry map[string]any, owned string, checkOnly bool) ([]byte, bool, bool, error) {
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
				entry = mergeEntry(agent, fields, entry, owned)
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

// mergeEntry keeps user settings when updating an entry on the same transport.
func mergeEntry(agent Agent, previous, desired map[string]any, owned string) map[string]any {
	launch := []string{"command", "args", "url", "type"}
	_, oldURL := previous["url"]
	_, newURL := desired["url"]
	oldType, _ := previous["type"].(string)
	newType, _ := desired["type"].(string)
	if oldURL != newURL || (agent == AgentClaude && oldURL && oldType != newType) {
		return desired
	}
	merged := maps.Clone(previous)
	maps.DeleteFunc(merged, func(k string, v any) bool {
		return slices.Contains(launch, k) || k == "bearer_token_env_var" && v == owned
	})
	maps.Copy(merged, desired)
	_, bearer := desired["bearer_token_env_var"]
	for _, field := range []string{"env", "headers", "http_headers", "env_http_headers"} {
		values := map[string]any{}
		saved, _ := previous[field].(map[string]any)
		maps.Copy(values, saved)
		claudeHeaders := agent == AgentClaude && field == "headers"
		codexBearer := strings.HasSuffix(field, "http_headers") && bearer
		maps.DeleteFunc(values, func(k string, v any) bool {
			return strings.EqualFold(k, "Authorization") && (codexBearer || claudeHeaders && v == "Bearer ${"+owned+"}")
		})
		supplied, ok := desired[strings.TrimPrefix(field, "env_")].(map[string]string)
		for key, value := range supplied {
			for old := range values {
				if field != "env" && strings.EqualFold(old, key) {
					delete(values, old)
				}
			}
			if field != "env_http_headers" {
				values[key] = value
			}
		}
		if len(values) != 0 && (ok || claudeHeaders || codexBearer) {
			merged[field] = values
		} else if ok || claudeHeaders || codexBearer {
			delete(merged, field)
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
