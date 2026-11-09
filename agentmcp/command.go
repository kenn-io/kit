package agentmcp

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"go.kenn.io/kit/agenthook"
)

// Command renders a manual user registration for POSIX shells and PowerShell.
// Commands can contain credentials and must be handled like config files.
// Codex's CLI cannot express literal HTTP headers, so those return an error.
func Command(agent Agent, name string, server Server) (agenthook.Commands, error) {
	entry, err := prepareConfig(agent, name, server)
	if err != nil {
		return agenthook.Commands{}, err
	}
	if agent != AgentClaude && agent != AgentCodex {
		return agenthook.Commands{}, fmt.Errorf("manual MCP command unavailable for agent %q", agent)
	}
	args := []string{"mcp", "add"}
	if agent == AgentClaude {
		args = append(args, "--scope", "user")
	}
	if server.URL != "" {
		if agent == AgentCodex {
			if len(server.Headers) != 0 {
				return agenthook.Commands{}, errors.New("codex mcp add cannot express literal HTTP headers")
			}
			args = append(args, "--url", server.URL)
			if server.BearerTokenEnvVar != "" {
				args = append(args, "--bearer-token-env-var", server.BearerTokenEnvVar)
			}
		} else {
			args = append(args, "--transport", entry["type"].(string))
			headers, _ := entry["headers"].(map[string]string)
			for _, key := range slices.Sorted(maps.Keys(headers)) {
				args = append(args, "--header", key+": "+headers[key])
			}
		}
		args = append(args, "--", name)
		if agent == AgentClaude {
			args = append(args, server.URL)
		}
	} else {
		if agent == AgentClaude {
			args = append(args, name)
		}
		for _, key := range slices.Sorted(maps.Keys(server.Env)) {
			args = append(args, "--env", key+"="+server.Env[key])
		}
		if agent == AgentCodex {
			args = append(args, name)
		}
		args = append(args, "--", server.Command)
		args = append(args, server.Args...)
	}
	return agenthook.BuildCommand(string(agent), args...)
}
