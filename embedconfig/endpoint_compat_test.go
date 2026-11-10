package embedconfig_test

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
)

// Fixed expectations recorded before the endpointpolicy extraction. Wrapper
// equivalence alone cannot detect a change shared by both entry points.
func TestCanonicalEndpointCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want, wantErr string
		trust                    bool
	}{
		{name: "canonical", raw: " HTTPS://Example.TEST:443/v1/// ", want: "https://example.test/v1"},
		{name: "escaped path", raw: "https://example.test/a%2Fb/", want: "https://example.test/a%2Fb"},
		{name: "empty query", raw: "https://example.test/?", want: "https://example.test"},
		{name: "empty fragment", raw: "https://example.test/#", want: "https://example.test"},
		{name: "zone", raw: "https://[FE80::1%Eth0]:443/v1/", want: "https://[fe80::1%25Eth0]/v1"},
		{name: "mapped loopback", raw: "http://[::ffff:127.0.0.1]:80/", want: "http://[::ffff:127.0.0.1]"},
		{name: "scheme first", raw: "ftp://user@example.test/?a=1", wantErr: "embed endpoint must use http or https"},
		{name: "empty", wantErr: "embed endpoint must use http or https"},
		{name: "no host", raw: "https://:443/", wantErr: "embed endpoint must include a host"},
		{name: "userinfo first", raw: "http://user@example.test/?q=1", wantErr: "embed endpoint must not include userinfo"},
		{name: "query first", raw: "http://example.test/../?q=1", wantErr: "embed endpoint must not include a query or fragment"},
		{name: "fragment", raw: "https://example.test/#fragment", wantErr: "embed endpoint must not include a query or fragment"},
		{name: "parent first", raw: "http://example.test/%2e%2e/", wantErr: "embed endpoint path must not contain a parent segment"},
		{name: "private refused", raw: "http://10.0.0.1/", wantErr: "embed plaintext http requires loopback or an explicitly trusted private address"},
		{name: "name trusted", raw: "http://SERVICE:7777/api/", trust: true, want: "http://service:7777/api"},
		{name: "public refused", raw: "http://8.8.8.8/", trust: true, wantErr: "embed plaintext http requires loopback or an explicitly trusted private address"},
		{name: "malformed", raw: "https://[::1", wantErr: `embed endpoint is invalid: parse "https://[::1": missing ']' in host`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := embedconfig.CanonicalEndpoint(tc.raw, tc.trust)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOriginCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		u    *url.URL
		want string
	}{
		{name: "nil"},
		{name: "missing scheme", u: &url.URL{Host: "example.test"}},
		{name: "port only", u: &url.URL{Scheme: "https", Host: ":443"}},
		{name: "permissive origin", u: &url.URL{Scheme: "ftp", Host: "EXAMPLE.TEST:21", Path: "/../", RawQuery: "q=1"}, want: "ftp://example.test:21"},
		{name: "zone percent", u: &url.URL{Scheme: "https", Host: "[FE80::1%Eth%2]:443"}, want: "https://[fe80::1%25Eth%252]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := embedconfig.Origin(tc.u)
			if tc.want == "" {
				require.EqualError(t, err, "embed endpoint origin is missing")
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestCanonicalEndpointParseErrorCompatibility(t *testing.T) {
	for _, raw := range []string{"https://example.test/%zz", "https://[fe80::1%Eth0]/%zz"} {
		t.Run(raw, func(t *testing.T) {
			_, err := embedconfig.CanonicalEndpoint(raw, false)
			var parseErr *url.Error
			require.ErrorAs(t, err, &parseErr)
			assert.Equal(t, raw, parseErr.URL)
			var escapeErr url.EscapeError
			require.ErrorAs(t, err, &escapeErr)
			wantEscape := "%zz"
			if raw == "https://[fe80::1%Eth0]/%zz" {
				wantEscape = "%Et" // Original parse error, before rewriting the zone.
			}
			assert.Equal(t, url.EscapeError(wantEscape), escapeErr)
			assert.Equal(t, "embed endpoint is invalid: "+parseErr.Error(), err.Error())
		})
	}
}
