// Package mcpidentity shares application-supplied MCP display metadata and a
// PNG asset across stdio and HTTP. It performs no external requests.
package mcpidentity

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image/png"
	"net/http"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Identity holds an immutable snapshot of public application metadata and PNG
// bytes. Construct it once with New; it is safe for concurrent use.
type Identity struct {
	info mcp.Implementation
	icon mcp.Icon
	png  []byte
}

// New snapshots info and iconPNG. The PNG header must contain valid dimensions.
// Its icon replaces info.Icons with a self-contained data URI, so clients do not
// need HTTP access or credentials to display it. Other metadata is preserved.
// Assets must be application-controlled public data, never user uploads or
// secrets. Kit does not fetch or rewrite the website URL or image bytes.
func New(info mcp.Implementation, iconPNG []byte) (*Identity, error) {
	config, err := png.DecodeConfig(bytes.NewReader(iconPNG))
	if err != nil {
		return nil, fmt.Errorf("MCP identity PNG header: %w", err)
	}
	info.Icons = nil
	return &Identity{
		info: info,
		icon: mcp.Icon{
			Source:   "data:image/png;base64," + base64.StdEncoding.EncodeToString(iconPNG),
			MIMEType: "image/png",
			Sizes:    []string{strconv.Itoa(config.Width) + "x" + strconv.Itoa(config.Height)},
		},
		png: bytes.Clone(iconPNG),
	}, nil
}

// Implementation returns a fresh SDK value for mcp.NewServer, usable unchanged
// by stdio and HTTP. Mutating it cannot affect this Identity or another caller.
// The SDK handles initialize serverInfo and modern per-result serverInfo metadata.
func (i *Identity) Implementation() *mcp.Implementation {
	info := i.info
	icon := i.icon
	icon.Sizes = append([]string(nil), icon.Sizes...)
	info.Icons = []mcp.Icon{icon}
	return &info
}

// IconHandler serves GET and HEAD /icon.png and /favicon.ico with identical PNG
// bytes. Other paths return 404 and other methods return 405. Mount these exact
// routes separately from the authenticated MCP endpoint. This public handler
// serves only the supplied image; it never creates an MCP server or reads files.
// Applications retain listener, host, TLS and routing policy.
func (i *Identity) IconHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/icon.png" && r.URL.Path != "/favicon.ico" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(len(i.png)))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		if r.Method == http.MethodGet {
			_, _ = w.Write(i.png)
		}
	})
}
