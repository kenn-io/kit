package httpproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func listen(t *testing.T, cfg Config) *Proxy {
	t.Helper()
	p, err := Listen(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, p.Close()) })
	return p
}

func newClient(t *testing.T, p *Proxy) *Client {
	t.Helper()
	c, err := p.NewClient()
	require.NoError(t, err)
	return c
}

func bind(t *testing.T, c *Client, ctx context.Context) {
	t.Helper()
	stop, err := c.Bind(ctx)
	require.NoError(t, err)
	t.Cleanup(stop)
}

func host(server *httptest.Server) string {
	return strings.TrimPrefix(strings.TrimPrefix(server.URL, "http://"), "https://")
}

func httpClient(server *httptest.Server, proxy *url.URL) *http.Client {
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxy)
	return &http.Client{Transport: transport}
}

func send(t *testing.T, client *http.Client, target string) (*http.Response, error) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	return client.Do(request)
}

func get(t *testing.T, client *http.Client, target string) (int, string) {
	t.Helper()
	resp, err := send(t, client, target)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

// stall sends one byte of a large body, then holds the response open until
// the client goes away.
func stall(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "1000000")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte{'x'}); err != nil {
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	<-r.Context().Done()
}

func reply(w http.ResponseWriter, r *http.Request) {
	_, _ = io.WriteString(w, "hello "+r.URL.Path)
}

func TestForwardsPlainHTTPAndTunnelsTLS(t *testing.T) {
	var sawProxyAuth atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			sawProxyAuth.Store(true)
		}
		reply(w, r)
	})
	plain := httptest.NewServer(handler)
	defer plain.Close()
	secure := httptest.NewTLSServer(handler)
	defer secure.Close()
	p := listen(t, Config{Allow: []string{host(plain), host(secure)}})
	c := newClient(t, p)
	bind(t, c, t.Context())

	for _, server := range []*httptest.Server{plain, secure} {
		status, body := get(t, httpClient(server, c.URL()), server.URL+"/object")
		assert.Equal(t, http.StatusOK, status)
		assert.Equal(t, "hello /object", body)
	}
	assert.False(t, sawProxyAuth.Load(), "proxy credentials must not reach the upstream")
}

func TestRefusesRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(reply))
	defer upstream.Close()
	other := httptest.NewServer(http.HandlerFunc(reply))
	defer other.Close()
	var dials atomic.Int64
	p := listen(t, Config{
		Allow: []string{host(upstream)},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
	})
	bound := newClient(t, p)
	bind(t, bound, t.Context())
	unbound := newClient(t, p)
	closed := newClient(t, p)
	bind(t, closed, t.Context())
	require.NoError(t, closed.Close())
	wrongPassword := bound.URL()
	wrongPassword.User = url.UserPassword(bound.username, "wrong")
	anonymous := bound.URL()
	anonymous.User = nil

	tests := []struct {
		name   string
		proxy  *url.URL
		target *httptest.Server
		want   int
	}{
		{"no credentials", anonymous, upstream, http.StatusProxyAuthRequired},
		{"wrong password", wrongPassword, upstream, http.StatusProxyAuthRequired},
		{"closed client", closed.URL(), upstream, http.StatusProxyAuthRequired},
		{"address not allowed", bound.URL(), other, http.StatusForbidden},
		{"unbound client", unbound.URL(), upstream, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, _ := get(t, httpClient(tt.target, tt.proxy), tt.target.URL)
			assert.Equal(t, tt.want, status)
			status, err := connect(t, p, tt.proxy, host(tt.target))
			require.NoError(t, err)
			assert.Equal(t, tt.want, status)
		})
	}
	assert.Zero(t, dials.Load(), "a refused request must not reach an upstream")
}

// connect sends one CONNECT request and returns the proxy's status, or the
// error reading it.
func connect(t *testing.T, p *Proxy, proxy *url.URL, target string) (int, error) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", p.Addr())
	require.NoError(t, err)
	defer conn.Close()
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		request += "Proxy-Authorization: Basic " + credentials + "\r\n"
	}
	_, err = io.WriteString(conn, request+"\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

// A stalled transfer would block these tests forever if the cutoff failed.
func TestBindingEndCutsOffTransfers(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(stall))
	defer plain.Close()
	secure := httptest.NewTLSServer(http.HandlerFunc(stall))
	defer secure.Close()
	p := listen(t, Config{Allow: []string{host(plain), host(secure)}})

	for _, server := range []*httptest.Server{plain, secure} {
		t.Run(server.URL, func(t *testing.T) {
			c := newClient(t, p)
			client := httpClient(server, c.URL())
			ctx, cancel := context.WithCancel(t.Context())
			stop, err := c.Bind(ctx)
			require.NoError(t, err)
			defer stop()

			resp, err := send(t, client, server.URL)
			require.NoError(t, err)
			first := make([]byte, 1)
			_, err = io.ReadFull(resp.Body, first)
			require.NoError(t, err, "the proxy streams the first byte before the cutoff")
			cancel()
			_, err = io.Copy(io.Discard, resp.Body)
			require.NoError(t, resp.Body.Close())
			require.Error(t, err, "the stalled transfer must be cut off")

			resp, err = send(t, client, server.URL)
			if err == nil {
				assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
				require.NoError(t, resp.Body.Close())
			}

			client.CloseIdleConnections()
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			stop, err = c.Bind(ctx)
			require.NoError(t, err, "a client can bind again after a cutoff")
			defer stop()
			resp, err = send(t, client, server.URL)
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		})
	}
}

func TestBindingEndCutsOffDial(t *testing.T) {
	const target = "192.0.2.1:443"
	dialing := make(chan struct{})
	p := listen(t, Config{
		Allow: []string{target},
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			close(dialing)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	c := newClient(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	bind(t, c, ctx)
	go func() {
		<-dialing
		cancel()
	}()

	status, err := connect(t, p, c.URL(), target)
	if err == nil {
		assert.Equal(t, http.StatusBadGateway, status, "the tunnel must not open")
	}
}

func TestStopRefusesNewRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(reply))
	defer upstream.Close()
	p := listen(t, Config{Allow: []string{host(upstream)}})
	c := newClient(t, p)
	client := httpClient(upstream, c.URL())

	stop, err := c.Bind(t.Context())
	require.NoError(t, err)
	_, err = c.Bind(t.Context())
	require.Error(t, err, "a client has one binding at a time")
	status, _ := get(t, client, upstream.URL)
	require.Equal(t, http.StatusOK, status)

	stop()
	status, _ = get(t, client, upstream.URL)
	assert.Equal(t, http.StatusServiceUnavailable, status)
}

func TestCloseEndsTunnels(t *testing.T) {
	secure := httptest.NewTLSServer(http.HandlerFunc(stall))
	defer secure.Close()
	p, err := Listen(Config{Allow: []string{host(secure)}})
	require.NoError(t, err)
	c := newClient(t, p)
	bind(t, c, t.Context())
	resp, err := send(t, httpClient(secure, c.URL()), secure.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.NoError(t, p.Close())
	_, err = io.Copy(io.Discard, resp.Body)
	require.Error(t, err)
	_, err = p.NewClient()
	require.ErrorIs(t, err, ErrClosed)
	_, err = c.Bind(t.Context())
	require.ErrorIs(t, err, ErrClosed)
}

func TestListenRejectsAllowlist(t *testing.T) {
	for _, allow := range [][]string{nil, {"example.com"}, {":443"}, {"example.com:"}} {
		_, err := Listen(Config{Allow: allow})
		assert.Error(t, err, "%q", allow)
	}
}

// A cutoff callback that started before stop and a new Bind runs late; it
// must leave the new binding's transfers open.
func TestStaleCutoffSparesNewerBinding(t *testing.T) {
	more := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "2")
		controller := http.NewResponseController(w)
		if _, err := w.Write([]byte{'a'}); err != nil || controller.Flush() != nil {
			return
		}
		select {
		case <-more:
			_, _ = w.Write([]byte{'b'})
		case <-r.Context().Done():
		}
	})
	plain := httptest.NewServer(handler)
	defer plain.Close()
	secure := httptest.NewTLSServer(handler)
	defer secure.Close()
	p := listen(t, Config{Allow: []string{host(plain), host(secure)}})
	defer close(more)

	for _, server := range []*httptest.Server{plain, secure} {
		t.Run(server.URL, func(t *testing.T) {
			c := newClient(t, p)
			stop, err := c.Bind(t.Context())
			require.NoError(t, err)
			p.mu.Lock()
			stale := c.binding
			p.mu.Unlock()
			stop()
			bind(t, c, t.Context())

			resp, err := send(t, httpClient(server, c.URL()), server.URL)
			require.NoError(t, err)
			defer resp.Body.Close()
			first := make([]byte, 1)
			_, err = io.ReadFull(resp.Body, first)
			require.NoError(t, err)
			c.cutOff(stale)
			more <- struct{}{}
			rest, err := io.ReadAll(resp.Body)
			require.NoError(t, err, "the stale cutoff closed the newer binding's transfer")
			assert.Equal(t, "ab", string(first)+string(rest))
		})
	}
}

func TestShutdownClosesUpstreamConnections(t *testing.T) {
	tests := []struct {
		name     string
		shutdown func(p *Proxy, c *Client, cancel context.CancelFunc)
	}{
		{"binding ends", func(_ *Proxy, _ *Client, cancel context.CancelFunc) { cancel() }},
		{"client closes", func(_ *Proxy, c *Client, _ context.CancelFunc) { assert.NoError(t, c.Close()) }},
		{"proxy closes", func(p *Proxy, _ *Client, _ context.CancelFunc) { assert.NoError(t, p.Close()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			closed := make(chan struct{}, 4)
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(reply))
			upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateClosed {
					closed <- struct{}{}
				}
			}
			upstream.Start()
			defer upstream.Close()
			p := listen(t, Config{Allow: []string{host(upstream)}})
			c := newClient(t, p)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			bind(t, c, ctx)
			status, _ := get(t, httpClient(upstream, c.URL()), upstream.URL)
			require.Equal(t, http.StatusOK, status)

			tt.shutdown(p, c, cancel)
			<-closed
		})
	}
}
