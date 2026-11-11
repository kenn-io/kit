// Package testserver provides the app-server exchange shared by test processes.
package testserver

import (
	"bufio"
	"cmp"
	"encoding/json/v2"
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// WaitForInterrupt signals that a helper is blocked until it is interrupted.
func WaitForInterrupt(marker string) error {
	if err := os.WriteFile(marker, []byte("blocked"), 0o600); err != nil {
		return err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	<-signals
	signal.Stop(signals)
	return nil
}

// InstallExecutable copies the current test process onto PATH under name and
// returns its original path. TestMain owns the copied process's behavior.
func InstallExecutable(tb testing.TB, name string) string {
	tb.Helper()
	executable, err := os.Executable()
	require.NoError(tb, err)
	program, err := os.ReadFile(executable)
	require.NoError(tb, err)
	dir := tb.TempDir()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(tb, os.WriteFile(filepath.Join(dir, name), program, 0o700))
	tb.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return executable
}

// Request is one request or notification from the client under test.
type Request struct {
	ID     int            `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// Read decodes the next app-server frame.
func Read(input *bufio.Scanner) (Request, error) {
	if !input.Scan() {
		return Request{}, cmp.Or(input.Err(), io.EOF)
	}
	var request Request
	err := json.Unmarshal(input.Bytes(), &request)
	return request, err
}

// Handshake checks initialization and sends unrelated frames before its reply.
func Handshake(input *bufio.Scanner, output io.Writer) error {
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		return errors.New("expected app-server --stdio")
	}
	request, err := Read(input)
	if err != nil {
		return err
	}
	info, _ := request.Params["clientInfo"].(map[string]any)
	caps, _ := request.Params["capabilities"].(map[string]any)
	if request.ID != 1 || request.Method != "initialize" || info["name"] != "kit-agent-check" || caps["experimentalApi"] != true {
		return errors.New("invalid initialization request")
	}
	if _, err := io.WriteString(output, "{\"method\":\"notice\",\"params\":{}}\n{\"id\":999,\"result\":{}}\n{\"id\":1,\"result\":{}}\n"); err != nil {
		return err
	}
	request, err = Read(input)
	if err != nil {
		return err
	}
	if request.ID != 0 || request.Method != "initialized" || len(request.Params) != 0 {
		return errors.New("invalid initialized notification")
	}
	return nil
}
