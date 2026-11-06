package mcpidentity_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/mcphttp"
	"go.kenn.io/kit/mcpidentity"
)

func fixturePNG(tb testing.TB, red, green, blue uint8) []byte {
	tb.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: red, G: green, B: blue, A: 255})
	var out bytes.Buffer
	require.NoError(tb, png.Encode(&out, img))
	return out.Bytes()
}

func TestRejectInvalidPNG(t *testing.T) {
	for _, data := range [][]byte{nil, {}, []byte("not an image"), []byte("\x89PNG\r\n\x1a\n")} {
		_, err := mcpidentity.New(mcp.Implementation{Name: "fixture", Version: "1"}, data)
		require.Error(t, err)
	}
}

// The advertised data URI must round-trip to the caller's exact PNG, even if
// the caller later changes its input or a previously returned SDK value.
func FuzzIdentitySnapshot(f *testing.F) {
	f.Add(uint8(0), uint8(0), uint8(0), "fixture", "https://example.invalid")
	f.Add(uint8(255), uint8(128), uint8(1), "another app", "")
	f.Fuzz(func(t *testing.T, red, green, blue uint8, name, website string) {
		data := fixturePNG(t, red, green, blue)
		original := bytes.Clone(data)
		info := mcp.Implementation{Name: name, Version: "1", Title: "Fixture", WebsiteURL: website}
		identity, err := mcpidentity.New(info, data)
		require.NoError(t, err)
		data[0] = 0
		first := identity.Implementation()
		require.Len(t, first.Icons, 1)
		first.Name = "changed"
		first.Icons[0].Source = "changed"
		first.Icons[0].Sizes[0] = "changed"
		got := identity.Implementation()
		assert.Equal(t, info.Name, got.Name)
		assert.Equal(t, info.WebsiteURL, got.WebsiteURL)
		assert.Equal(t, info.Title, got.Title)
		require.Len(t, got.Icons, 1)
		assert.Equal(t, "image/png", got.Icons[0].MIMEType)
		assert.Equal(t, []string{"1x1"}, got.Icons[0].Sizes)
		const prefix = "data:image/png;base64,"
		require.True(t, strings.HasPrefix(got.Icons[0].Source, prefix))
		decoded, err := base64.StdEncoding.DecodeString(got.Icons[0].Source[len(prefix):])
		require.NoError(t, err)
		assert.Equal(t, original, decoded)
		imageResponse := httptest.NewRecorder()
		identity.IconHandler().ServeHTTP(imageResponse, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/icon.png", nil))
		assert.Equal(t, original, imageResponse.Body.Bytes())
	})
}

type bearerTransport struct{ base http.RoundTripper }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer fixture-token")
	return b.base.RoundTrip(r)
}

func TestIdentityAcrossTransportsAndPublicImageBoundary(t *testing.T) {
	data := fixturePNG(t, 1, 2, 3)
	identity, err := mcpidentity.New(mcp.Implementation{
		Name: "fixture", Version: "1", WebsiteURL: "https://example.invalid",
	}, data)
	require.NoError(t, err)
	var factories atomic.Int32
	endpoint, err := mcphttp.New(func(*http.Request) *mcp.Server {
		factories.Add(1)
		return mcp.NewServer(identity.Implementation(), nil)
	}, mcphttp.Options{Authenticate: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Detach cleanup from test cancellation, then bound draining.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, endpoint.Shutdown(ctx))
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", endpoint)
	mux.Handle("/icon.png", identity.IconHandler())
	mux.Handle("/favicon.ico", identity.IconHandler())
	host := httptest.NewServer(mux)
	t.Cleanup(host.Close)
	for _, path := range []string{"/icon.png", "/favicon.ico"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			req, err := http.NewRequestWithContext(t.Context(), method, host.URL+path, nil)
			require.NoError(t, err)
			res, err := host.Client().Do(req)
			require.NoError(t, err)
			body, err := io.ReadAll(res.Body)
			require.NoError(t, res.Body.Close())
			require.NoError(t, err)
			assert.Equal(t, http.StatusOK, res.StatusCode)
			assert.Equal(t, "image/png", res.Header.Get("Content-Type"))
			assert.Equal(t, "nosniff", res.Header.Get("X-Content-Type-Options"))
			assert.Equal(t, "public, max-age=3600", res.Header.Get("Cache-Control"))
			assert.Equal(t, int64(len(data)), res.ContentLength)
			if method == http.MethodGet {
				assert.Equal(t, data, body)
			} else {
				assert.Empty(t, body)
			}
		}
	}
	unauthorized := httptest.NewRecorder()
	mux.ServeHTTP(unauthorized, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil))
	assert.Equal(t, http.StatusUnauthorized, unauthorized.Code)
	assert.Zero(t, factories.Load(), "public image routes and failed auth must not create MCP servers")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/icon.png", http.StatusMethodNotAllowed},
		{http.MethodGet, "/other", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		identity.IconHandler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))
		assert.Equal(t, tc.status, rec.Code)
	}

	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		for _, transport := range []string{"stdio", "http"} {
			t.Run(version+"/"+transport, func(t *testing.T) {
				var wire mcp.Transport
				if transport == "stdio" {
					// Exercise the same newline JSON IO transport as stdio without
					// starting a subprocess or touching the user's standard streams.
					clientPipe, serverPipe := net.Pipe()
					session, err := mcp.NewServer(identity.Implementation(), nil).Connect(t.Context(),
						&mcp.IOTransport{Reader: serverPipe, Writer: serverPipe}, nil)
					require.NoError(t, err)
					t.Cleanup(func() { assert.NoError(t, session.Close()) })
					wire = &mcp.IOTransport{Reader: clientPipe, Writer: clientPipe}
				} else {
					wire = &mcp.StreamableClientTransport{
						Endpoint:             host.URL + "/mcp",
						DisableStandaloneSSE: true,
						HTTPClient:           &http.Client{Transport: bearerTransport{base: host.Client().Transport}},
					}
				}
				client := mcp.NewClient(&mcp.Implementation{Name: "fixture-client", Version: "1"}, nil)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				session, err := client.Connect(ctx, wire, &mcp.ClientSessionOptions{ProtocolVersion: version})
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, session.Close()) })
				require.NotNil(t, session.InitializeResult())
				assert.Equal(t, identity.Implementation(), session.InitializeResult().ServerInfo)
			})
		}
	}
}
