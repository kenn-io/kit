package endpointpolicy_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/endpointpolicy"
)

func TestCanonicalEndpointAddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		host               string
		untrusted, trusted bool
	}{
		{"localhost", true, true},
		{"LOCALHOST", true, true},
		{"127.0.0.1", true, true},
		{"127.255.255.255", true, true},
		{"[::1]", true, true},
		{"[::ffff:127.0.0.1]", true, true},
		{"service", false, true},
		{"example.com", false, true},
		{"localhost.", false, true},
		{"0.0.0.0", false, true},
		{"[::]", false, true},
		{"10.0.0.0", false, true},
		{"10.255.255.255", false, true},
		{"172.16.0.0", false, true},
		{"172.31.255.255", false, true},
		{"192.168.0.0", false, true},
		{"[fc00::]", false, true},
		{"[fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]", false, true},
		{"169.254.0.0", false, true},
		{"169.254.255.255", false, true},
		{"[fe80::1%25Eth0]", false, true},
		{"[febf:ffff::1]", false, true},
		{"100.64.0.0", false, true},
		{"100.127.255.255", false, true},
		{"[::ffff:10.0.0.1]", false, true},
		{"[::ffff:100.64.0.1]", false, true},
		{"100.63.255.255", false, false},
		{"100.128.0.0", false, false},
		{"172.15.255.255", false, false},
		{"172.32.0.0", false, false},
		{"169.253.255.255", false, false},
		{"169.255.0.0", false, false},
		{"8.8.8.8", false, false},
		{"[2001:4860:4860::8888]", false, false},
		{"[::ffff:8.8.8.8]", false, false},
		{"224.0.0.1", false, false},
		{"255.255.255.255", false, false},
		{"[ff02::1]", false, false},
	} {
		for _, trust := range []bool{false, true} {
			for _, scheme := range []string{"http", "https"} {
				raw := scheme + "://" + tc.host + "/v1"
				t.Run(raw+map[bool]string{false: "/untrusted", true: "/trusted"}[trust], func(t *testing.T) {
					wantAllowed := tc.untrusted || trust && tc.trusted || scheme == "https"
					got, err := endpointpolicy.CanonicalEndpoint(raw, trust, "catalog")
					if !wantAllowed {
						require.EqualError(t, err, "catalog plaintext http requires loopback or an explicitly trusted private address")
						assert.Empty(t, got)
						return
					}
					require.NoError(t, err)
					assert.Equal(t, strings.ReplaceAll(raw, "LOCALHOST", "localhost"), got)
				})
			}
		}
	}
}

func TestCanonicalEndpointSubjects(t *testing.T) {
	for _, subject := range []string{"catalog", "integrations.tasks", ""} {
		prefix := subject
		if prefix != "" {
			prefix += " "
		}
		for _, tc := range []struct{ raw, suffix string }{
			{"ftp://example.test", "endpoint must use http or https"},
			{"https://:443", "endpoint must include a host"},
			{"https://user@example.test", "endpoint must not include userinfo"},
			{"https://example.test?q=1", "endpoint must not include a query or fragment"},
			{"https://example.test#value", "endpoint must not include a query or fragment"},
			{"https://example.test/%2e%2e", "endpoint path must not contain a parent segment"},
			{"http://service", "plaintext http requires loopback or an explicitly trusted private address"},
			{"https://[::1", `endpoint is invalid: parse "https://[::1": missing ']' in host`},
		} {
			t.Run(subject+"/"+tc.raw, func(t *testing.T) {
				got, err := endpointpolicy.CanonicalEndpoint(tc.raw, false, subject)
				require.EqualError(t, err, prefix+tc.suffix)
				assert.Empty(t, got)
			})
		}
	}
}

func TestCanonicalEndpointCanonicalization(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{" HTTPS://Example.TEST:443/a%2Fb/// ", "https://example.test/a%2Fb"},
		{"http://LOCALHOST:80/", "http://localhost"},
		{"https://example.test:8443/v1..2/", "https://example.test:8443/v1..2"},
		{"https://[FE80::1%Eth0]/", "https://[fe80::1%25Eth0]"},
		{"https://[fe80::1%25Eth%252]/", "https://[fe80::1%25Eth%252]"},
		{"https://example.test/?", "https://example.test"},
		{"https://example.test/#", "https://example.test"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := endpointpolicy.CanonicalEndpoint(tc.raw, false, "catalog")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOrigin(t *testing.T) {
	for _, subject := range []string{"catalog", ""} {
		prefix := subject
		if prefix != "" {
			prefix += " "
		}
		for _, u := range []*url.URL{nil, {}, {Host: "example.test"}, {Scheme: "https", Host: ":443"}} {
			got, err := endpointpolicy.Origin(u, subject)
			require.EqualError(t, err, prefix+"endpoint origin is missing")
			assert.Empty(t, got)
		}
	}
	for _, tc := range []struct {
		u    *url.URL
		want string
	}{
		{&url.URL{Scheme: "https", Host: "EXAMPLE.TEST:443", Path: "/v1"}, "https://example.test"},
		{&url.URL{Scheme: "http", Host: "localhost:8080"}, "http://localhost:8080"},
		{&url.URL{Scheme: "https", Host: "[FE80::1%Eth%2]:443"}, "https://[fe80::1%25Eth%252]"},
		{&url.URL{Scheme: "ftp", Host: "EXAMPLE.TEST:21", Path: "/..", RawQuery: "q=1"}, "ftp://example.test:21"},
		{&url.URL{Scheme: "HTTPS", Host: "EXAMPLE.TEST:443"}, "https://example.test:443"},
	} {
		got, err := endpointpolicy.Origin(tc.u, "catalog")
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}
