package agentmcp

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Transport selects an MCP connection protocol.
type Transport string

const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
	TransportSSE   Transport = "sse"
)

// Server describes a stdio command or remote endpoint. With no Transport,
// URL selects streamable HTTP and Command selects stdio. Use TransportSSE
// explicitly for SSE endpoints; this package never guesses from a URL suffix.
// Command, Args, and Env belong to stdio; URL and Headers belong to HTTP.
type Server struct {
	Transport Transport
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
}

func validateURL(value string) error {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("MCP URL must be an absolute HTTP or HTTPS endpoint")
	}
	return nil
}

func (server Server) native(agent Agent) (map[string]any, error) {
	entry := map[string]any{}
	transport := server.Transport
	if transport == "" {
		transport = TransportStdio
		if server.URL != "" {
			transport = TransportHTTP
		}
	}
	if transport != TransportStdio && transport != TransportHTTP && transport != TransportSSE {
		return nil, fmt.Errorf("unsupported MCP transport %q", transport)
	}
	if transport == TransportSSE && agent == AgentCodex {
		return nil, errors.New("the Codex profile requires streamable HTTP or stdio")
	}
	if server.URL != "" {
		if transport == TransportStdio {
			return nil, errors.New("MCP stdio server cannot include a URL")
		}
		if server.Command != "" || len(server.Args) != 0 || len(server.Env) != 0 {
			return nil, errors.New("MCP HTTP server cannot include stdio fields")
		}
		if err := validateURL(server.URL); err != nil {
			return nil, err
		}
		urlKey, headerKey := "url", "headers"
		if (agent == AgentGemini || agent == AgentQwen) && transport == TransportHTTP {
			urlKey = "httpUrl"
		}
		if agent == AgentCodex {
			headerKey = "http_headers"
		}
		entry[urlKey] = server.URL
		if len(server.Headers) != 0 {
			entry[headerKey] = server.Headers
		}
		if agent == AgentClaude || agent == AgentDroid || agent == AgentCopilot {
			entry["type"] = string(transport)
		}
		if agent == AgentHermes && transport == TransportSSE {
			entry["transport"] = "sse"
		}
	} else {
		if transport != TransportStdio {
			return nil, errors.New("MCP remote server requires a URL")
		}
		if strings.TrimSpace(server.Command) == "" {
			return nil, errors.New("MCP server requires a command or URL")
		}
		if len(server.Headers) != 0 {
			return nil, errors.New("MCP stdio server cannot include HTTP headers")
		}
		entry["command"] = server.Command
		if len(server.Args) != 0 {
			entry["args"] = server.Args
		}
		if len(server.Env) != 0 {
			entry["env"] = server.Env
		}
		if agent == AgentClaude || agent == AgentDroid {
			entry["type"] = "stdio"
		}
		if agent == AgentCopilot {
			entry["type"] = "local"
		}
	}
	if agent == AgentCopilot {
		entry["tools"] = []string{"*"}
	}
	return entry, nil
}
