// Package daemon provides reusable building blocks for local CLI daemons.
package daemon

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// NetworkTCP identifies TCP endpoints.
	NetworkTCP = "tcp"
	// NetworkUnix identifies Unix-domain socket endpoints.
	NetworkUnix = "unix"
)

// MaxUnixPathLen is the platform socket path length limit.
// macOS/BSD: 104, Linux: 108.
var MaxUnixPathLen = func() int {
	if runtime.GOOS == "darwin" {
		return 104
	}
	return 108
}()

// Endpoint describes where a daemon listens and how clients should dial it.
type Endpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// TCPAddressPolicy validates TCP host:port strings while parsing endpoints.
type TCPAddressPolicy func(addr string) error

// ParseEndpointOptions configures ParseEndpoint.
type ParseEndpointOptions struct {
	// DefaultTCPAddress is used when the raw address is empty and no
	// DefaultUnixPath is set.
	DefaultTCPAddress string
	// DefaultUnixPath is used for an empty raw address or raw "unix://".
	DefaultUnixPath string
	// TCPPolicy validates TCP endpoints. Nil accepts any syntactically valid
	// TCP host:port.
	TCPPolicy TCPAddressPolicy
}

// ParseEndpoint parses "host:port", "http://host:port", or "unix:///path".
func ParseEndpoint(raw string, opts ParseEndpointOptions) (Endpoint, error) {
	if raw == "" {
		if opts.DefaultUnixPath != "" {
			return parseUnixEndpoint(opts.DefaultUnixPath)
		}
		raw = opts.DefaultTCPAddress
	}
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty daemon address")
	}
	if after, ok := strings.CutPrefix(raw, "http://"); ok {
		raw = after
	}
	if after, ok := strings.CutPrefix(raw, "unix://"); ok {
		if after == "" {
			after = opts.DefaultUnixPath
		}
		return parseUnixEndpoint(after)
	}
	return parseTCPEndpoint(raw, opts.TCPPolicy)
}

func parseTCPEndpoint(addr string, policy TCPAddressPolicy) (Endpoint, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return Endpoint{}, fmt.Errorf("invalid tcp daemon address %q: %w", addr, err)
	}
	if policy != nil {
		if err := policy(addr); err != nil {
			return Endpoint{}, err
		}
	}
	return Endpoint{Network: NetworkTCP, Address: addr}, nil
}

func parseUnixEndpoint(path string) (Endpoint, error) {
	if path == "" {
		return Endpoint{}, fmt.Errorf("empty unix socket path")
	}
	if !filepath.IsAbs(path) {
		return Endpoint{}, fmt.Errorf("unix socket path %q must be absolute", path)
	}
	if strings.ContainsRune(path, 0) {
		return Endpoint{}, fmt.Errorf("unix socket path contains null byte")
	}
	if len(path) >= MaxUnixPathLen {
		return Endpoint{}, fmt.Errorf(
			"unix socket path %q (%d bytes) exceeds platform limit of %d bytes",
			path, len(path), MaxUnixPathLen)
	}
	return Endpoint{Network: NetworkUnix, Address: path}, nil
}

// RequireLoopback rejects any TCP address whose host is not loopback.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse host:port: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("address %q is not a literal IP or localhost", addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("address %q is not loopback", addr)
	}
	return nil
}

// RequireNonPublic rejects public TCP addresses and unspecified binds.
func RequireNonPublic(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("address %q is not a literal IP", addr)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("address %q is unspecified; use a private address", addr)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnatBlock.Contains(ip) {
		return nil
	}
	return fmt.Errorf("address %q is public; use a private address", addr)
}

var cgnatBlock = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

// IsUnix reports whether e uses a Unix-domain socket.
func (e Endpoint) IsUnix() bool {
	return e.Network == NetworkUnix
}

// ConfigAddress returns a stable address string for config/runtime files.
func (e Endpoint) ConfigAddress() string {
	if e.IsUnix() {
		return "unix://" + e.Address
	}
	return e.Address
}

// BaseURL returns the HTTP base URL for daemon API requests.
func (e Endpoint) BaseURL() string {
	if e.IsUnix() {
		return "http://localhost"
	}
	return "http://" + e.Address
}

// Port returns the TCP port, or 0 for non-TCP endpoints.
func (e Endpoint) Port() int {
	if e.IsUnix() {
		return 0
	}
	_, portText, err := net.SplitHostPort(e.Address)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0
	}
	return port
}

// Listen binds a listener for the endpoint.
func (e Endpoint) Listen() (net.Listener, error) {
	if e.Network == "" {
		return nil, fmt.Errorf("empty daemon endpoint network")
	}
	if e.Address == "" {
		return nil, fmt.Errorf("empty daemon endpoint address")
	}
	return net.Listen(e.Network, e.Address)
}

// HTTPClientOptions configures HTTPClient.
type HTTPClientOptions struct {
	Timeout               time.Duration
	ResponseHeaderTimeout time.Duration
	DisableKeepAlives     bool
}

// HTTPClient returns a client configured to dial the endpoint.
func (e Endpoint) HTTPClient(opts HTTPClientOptions) *http.Client {
	client := &http.Client{Timeout: opts.Timeout}
	if e.IsUnix() {
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, NetworkUnix, e.Address)
			},
			DisableKeepAlives:     opts.DisableKeepAlives,
			Proxy:                 nil,
			ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		}
		return client
	}
	if opts.ResponseHeaderTimeout == 0 && !opts.DisableKeepAlives {
		return client
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return client
	}
	transport := base.Clone()
	transport.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
	transport.DisableKeepAlives = opts.DisableKeepAlives
	client.Transport = transport
	return client
}

// DefaultSocketPath returns a per-user socket path under XDG_RUNTIME_DIR when
// available, otherwise under os.TempDir().
func DefaultSocketPath(service string) string {
	if service == "" {
		service = "daemon"
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) {
		if info, err := os.Stat(xdg); err == nil && info.IsDir() {
			path := filepath.Join(xdg, service, "daemon.sock")
			if len(path) < MaxUnixPathLen {
				return path
			}
		}
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", service, runtimeUID()), "daemon.sock")
}
