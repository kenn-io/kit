// Package codexapp implements sequential calls to the local Codex app-server.
package codexapp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"time"

	"go.kenn.io/kit/consolewindow"
)

// Client owns one app-server child. Calls must be serialized.
// https://developers.openai.com/codex/app-server
type Client struct {
	close  func()
	input  io.Writer
	output *jsontext.Decoder
	id     int
}

// Start launches Codex and completes its initialization handshake.
func Start(ctx context.Context, executable, cwd string) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	cmd := consolewindow.CommandContext(ctx, executable, "app-server", "--stdio")
	cmd.Dir = cwd
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		cancel()
		return nil, err
	}
	// The npm launcher has a child process that inherits these pipes. Close
	// them on cancellation so a blocked RPC read does not outlive the launcher.
	kill := cmd.Cancel
	cmd.Cancel = func() error {
		_ = input.Close()
		_ = output.Close()
		return kill()
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	closeClient := func() {
		// EOF lets the npm launcher reap its child before cancellation.
		_ = input.Close()
		_ = cmd.Wait()
		cancel()
	}
	c := &Client{input: input, output: jsontext.NewDecoder(output)}
	if err := c.initialize(); err != nil {
		closeClient()
		return nil, err
	}
	c.close = closeClient
	return c, nil
}

func (c *Client) initialize() error {
	if err := c.Call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "kit-agent-check", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, nil); err != nil {
		return err
	}
	if _, err := io.WriteString(c.input, "{\"method\":\"initialized\",\"params\":{}}\n"); err != nil {
		return fmt.Errorf("codex initialized: %w", err)
	}
	return nil
}

// Call sends one request and skips notifications until its response arrives.
func (c *Client) Call(method string, params, result any) error {
	c.id++
	request, err := json.Marshal(map[string]any{"id": c.id, "method": method, "params": params})
	if err != nil {
		return err
	}
	if _, err := c.input.Write(append(request, '\n')); err != nil {
		return err
	}
	for {
		var response struct {
			ID     int            `json:"id"`
			Result jsontext.Value `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.UnmarshalDecode(c.output, &response); err != nil {
			return fmt.Errorf("codex %s: %w", method, err)
		}
		if response.ID != c.id {
			continue
		}
		if response.Error != nil {
			return fmt.Errorf("codex %s: %s", method, response.Error.Message)
		}
		if result == nil {
			return nil
		}
		if err := json.Unmarshal(response.Result, result); err != nil {
			return fmt.Errorf("codex %s response: %w", method, err)
		}
		return nil
	}
}

// Close sends EOF and waits for the child, bounded by the startup context.
func (c *Client) Close() { c.close() }
