package agentmcp

import (
	"bytes"
	"errors"
	"io"

	"github.com/BurntSushi/toml"
	"gopkg.in/yaml.v3"
)

func planDocument(agent Agent, original []byte, name string, entry map[string]any) ([]byte, bool, bool, error) {
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
		if fields, ok := previous.(map[string]any); agent == AgentCodex && ok {
			mergeCodexFields(fields, entry)
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
	var data []byte
	var err error
	if agent == AgentHermes {
		data, err = editYAML(original, name, entry, servers)
	} else {
		data, err = editTOML(original, name, entry)
	}
	return data, true, present, err
}

// mergeCodexFields keeps user-owned Codex settings that still apply to entry.
func mergeCodexFields(fields, entry map[string]any) {
	dropEmptyFields(fields)
	if fields["enabled"] == true {
		delete(fields, "enabled")
	}
	for key, value := range fields {
		switch key {
		case "command", "args", "env", "url", "http_headers", "bearer_token_env_var", "enabled":
			continue // Setup owns these fields.
		case "cwd", "env_vars":
			if entry["command"] == nil {
				continue
			}
		case "env_http_headers", "auth", "oauth", "oauth_resource", "http_headers_helper":
			if entry["url"] == nil {
				continue
			}
		}
		entry[key] = value
	}
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
