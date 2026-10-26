// Package httpproxy runs a loopback HTTP forward proxy that lets a Go program
// bound the network traffic of code it cannot cancel directly, such as an
// embedded C library or a child process that only accepts a proxy setting.
//
// Each Client has its own random proxy credentials. A Client opens new
// requests and tunnels only while it is bound to a context. When that context
// ends, the proxy closes every connection the Client holds, which stops
// transfers already in flight, and refuses new requests until the next Bind.
// Only allowlisted upstream addresses are reachable, so other local processes
// cannot use the proxy as a relay.
package httpproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrClosed reports use of a closed Proxy or Client.
var ErrClosed = errors.New("httpproxy: closed")

// Config configures a Proxy.
type Config struct {
	// Allow lists the upstream "host:port" addresses that clients may reach,
	// through CONNECT for TLS or absolute-form requests for plain HTTP. Hosts
	// compare case-insensitively. At least one address is required.
	Allow []string
	// Dial opens upstream connections. Nil uses a zero net.Dialer.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Proxy is a forward proxy listening on a loopback address.
type Proxy struct {
	listener net.Listener
	server   *http.Server
	allow    map[string]bool
	dial     func(ctx context.Context, network, address string) (net.Conn, error)
	forward  *httputil.ReverseProxy
	serving  chan error
	tunnels  sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	clients map[string]*Client
	owners  map[net.Conn]*Client
}

// Client is one caller's identity on a Proxy.
type Client struct {
	proxy    *Proxy
	username string
	password string

	// Guarded by proxy.mu.
	closed  bool
	binding *binding
	conns   map[net.Conn]struct{}
}

type binding struct {
	ctx  context.Context
	stop func() bool
}

type connKey struct{}

// Listen starts a Proxy on 127.0.0.1 with an ephemeral port.
func Listen(cfg Config) (*Proxy, error) {
	allow, err := allowlist(cfg.Allow)
	if err != nil {
		return nil, err
	}
	dial := cfg.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("httpproxy: listen on loopback: %w", err)
	}
	p := &Proxy{
		listener: listener,
		allow:    allow,
		dial:     dial,
		serving:  make(chan error, 1),
		clients:  make(map[string]*Client),
		owners:   make(map[net.Conn]*Client),
	}
	p.forward = &httputil.ReverseProxy{
		// The inbound request already carries the absolute upstream URL.
		Rewrite:   func(*httputil.ProxyRequest) {},
		Transport: &http.Transport{DialContext: dial},
		// Stream each upstream read at once; a slow response must reach the
		// client as it arrives, not when a buffer fills.
		FlushInterval: -1,
		ErrorHandler:  forwardError,
	}
	p.server = &http.Server{
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, conn)
		},
		ConnState: func(conn net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				p.untrack(conn)
			}
		},
	}
	go func() { p.serving <- p.server.Serve(listener) }()
	return p, nil
}

func allowlist(addresses []string) (map[string]bool, error) {
	if len(addresses) == 0 {
		return nil, errors.New("httpproxy: Config.Allow must name at least one upstream address")
	}
	allow := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		host, port, err := net.SplitHostPort(address)
		if err != nil || host == "" || port == "" {
			return nil, fmt.Errorf("httpproxy: allowed address %q must be host:port", address)
		}
		allow[net.JoinHostPort(strings.ToLower(host), port)] = true
	}
	return allow, nil
}

// Addr returns the proxy's "127.0.0.1:port" listening address.
func (p *Proxy) Addr() string { return p.listener.Addr().String() }

// NewClient creates a Client with random credentials. The Client is unbound.
func (p *Proxy) NewClient() (*Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, ErrClosed
	}
	c := &Client{proxy: p, username: rand.Text(), password: rand.Text(), conns: make(map[net.Conn]struct{})}
	p.clients[c.username] = c
	return c, nil
}

// Close stops the proxy, closes every connection, and waits for open tunnels
// to end. Clients become unusable.
func (p *Proxy) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	var conns []net.Conn
	for _, c := range p.clients {
		conns = append(conns, c.release()...)
	}
	p.mu.Unlock()
	closeAll(conns)
	err := p.server.Close()
	p.tunnels.Wait()
	if serveErr := <-p.serving; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}

// URL returns the proxy URL with this Client's credentials, in the form
// http://username:password@127.0.0.1:port.
func (c *Client) URL() *url.URL {
	return &url.URL{Scheme: "http", User: url.UserPassword(c.username, c.password), Host: c.proxy.Addr()}
}

// Bind lets the Client send requests until ctx ends or stop is called. When
// ctx ends first, the proxy closes every connection the Client holds. stop
// keeps idle connections open for reuse by a later Bind, but no new request
// or tunnel starts until then. A Client has at most one binding at a time.
func (c *Client) Bind(ctx context.Context) (stop func(), err error) {
	p := c.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.binding != nil {
		return nil, errors.New("httpproxy: client is already bound")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b := &binding{ctx: ctx}
	c.binding = b
	b.stop = context.AfterFunc(ctx, func() { c.cutOff(b) })
	return sync.OnceFunc(func() {
		b.stop()
		p.mu.Lock()
		defer p.mu.Unlock()
		if c.binding == b {
			c.binding = nil
		}
	}), nil
}

// Close revokes the Client's credentials and closes its connections.
func (c *Client) Close() error {
	p := c.proxy
	p.mu.Lock()
	if c.closed {
		p.mu.Unlock()
		return nil
	}
	delete(p.clients, c.username)
	conns := c.release()
	p.mu.Unlock()
	closeAll(conns)
	return nil
}

// release marks c closed and detaches its connections. p.mu must be held.
func (c *Client) release() []net.Conn {
	c.closed = true
	if c.binding != nil {
		c.binding.stop()
		c.binding = nil
	}
	return c.detachConns()
}

// detachConns removes c's connections from tracking. p.mu must be held.
func (c *Client) detachConns() []net.Conn {
	conns := make([]net.Conn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
		delete(c.proxy.owners, conn)
	}
	clear(c.conns)
	return conns
}

func (c *Client) cutOff(b *binding) {
	p := c.proxy
	p.mu.Lock()
	if c.binding == b {
		c.binding = nil
	}
	conns := c.detachConns()
	p.mu.Unlock()
	closeAll(conns)
}

// track records conn as c's while b is still c's live binding. It reports
// false when the binding ended or another Client owns conn, so the caller
// must not use conn.
func (c *Client) track(b *binding, conn net.Conn) bool {
	p := c.proxy
	p.mu.Lock()
	defer p.mu.Unlock()
	if c.binding != b || b.ctx.Err() != nil {
		return false
	}
	if owner, ok := p.owners[conn]; ok && owner != c {
		return false
	}
	c.conns[conn] = struct{}{}
	p.owners[conn] = c
	return true
}

func (p *Proxy) untrack(conn net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.owners[conn]; ok {
		delete(c.conns, conn)
		delete(p.owners, conn)
	}
}

func closeAll(conns []net.Conn) {
	for _, conn := range conns {
		// The connection is being abandoned; its close result changes nothing.
		_ = conn.Close()
	}
}

func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	c, b, err := p.authorize(r)
	if err != nil {
		status := http.StatusInternalServerError
		if refused, ok := errors.AsType[refusal](err); ok {
			status = int(refused)
		}
		if status == http.StatusProxyAuthRequired {
			w.Header().Set("Proxy-Authenticate", `Basic realm="httpproxy"`)
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	inbound, _ := r.Context().Value(connKey{}).(net.Conn)
	if inbound == nil || !c.track(b, inbound) {
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(b.ctx, cancel)()
	if r.Method == http.MethodConnect {
		p.tunnel(ctx, w, r, c, b)
		return
	}
	p.forward.ServeHTTP(w, r.WithContext(ctx))
}

// refusal is the HTTP status the proxy answers a refused request with.
type refusal int

func (r refusal) Error() string { return "httpproxy: " + http.StatusText(int(r)) }

// authorize returns the request's Client and its live binding, or a refusal.
func (p *Proxy) authorize(r *http.Request) (*Client, *binding, error) {
	username, password, ok := proxyCredentials(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return nil, nil, refusal(http.StatusProxyAuthRequired)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.clients[username]
	if c == nil || subtle.ConstantTimeCompare([]byte(password), []byte(c.password)) != 1 {
		return nil, nil, refusal(http.StatusProxyAuthRequired)
	}
	address, ok := upstreamAddress(r)
	if !ok || !p.allow[address] {
		return nil, nil, refusal(http.StatusForbidden)
	}
	if c.binding == nil {
		return nil, nil, refusal(http.StatusServiceUnavailable)
	}
	return c, c.binding, nil
}

func proxyCredentials(header string) (username, password string, ok bool) {
	scheme, encoded, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Basic") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", "", false
	}
	return strings.Cut(string(decoded), ":")
}

// upstreamAddress returns the normalized host:port a request targets.
func upstreamAddress(r *http.Request) (string, bool) {
	authority := r.Host
	if r.Method != http.MethodConnect {
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			return "", false
		}
		authority = r.URL.Host
		if r.URL.Port() == "" {
			authority = net.JoinHostPort(r.URL.Hostname(), "80")
		}
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return "", false
	}
	return net.JoinHostPort(strings.ToLower(host), port), true
}

func (p *Proxy) tunnel(ctx context.Context, w http.ResponseWriter, r *http.Request, c *Client, b *binding) {
	address, _ := upstreamAddress(r)
	upstream, err := p.dial(ctx, "tcp", address)
	if err != nil {
		http.Error(w, "dial upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !c.track(b, upstream) {
		_ = upstream.Close()
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}
	inbound, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		p.untrack(upstream)
		_ = upstream.Close()
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Hijacking hides the connection from the server, so the tunnel owns it.
	// Starting under p.mu orders the start before Close waits for tunnels.
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		_ = inbound.Close()
		_ = upstream.Close()
		return
	}
	p.tunnels.Go(func() {
		defer p.untrack(inbound)
		defer p.untrack(upstream)
		defer inbound.Close()
		defer upstream.Close()
		if _, err := io.WriteString(inbound, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		splice(inbound, buffered.Reader, upstream)
	})
}

// splice copies both directions until either side ends, then closes both.
func splice(inbound net.Conn, fromClient *bufio.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, fromClient)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(inbound, upstream)
		done <- struct{}{}
	}()
	<-done
	_ = inbound.Close()
	_ = upstream.Close()
	<-done
}

func forwardError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	if r.Context().Err() != nil {
		status = http.StatusServiceUnavailable
	}
	http.Error(w, err.Error(), status)
}
