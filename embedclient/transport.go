package embedclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"go.kenn.io/kit/embedconfig"
)

// ErrOrigin reports that a request left the configured endpoint origin.
var ErrOrigin = errors.New("embed request origin does not match the configured endpoint")

type originTransport struct {
	base   http.RoundTripper
	origin string
	apiKey string
}

func (t *originTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	origin, err := embedconfig.Origin(req.URL)
	if err != nil || origin != t.origin {
		return nil, ErrOrigin
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if t.apiKey == "" || req.Header.Get("Authorization") != "" {
		return base.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+t.apiKey)
	return base.RoundTrip(clone)
}

func pinClient(caller *http.Client, origin, apiKey string, timeout time.Duration) *http.Client {
	if caller == nil {
		caller = &http.Client{Timeout: timeout}
	}
	clone := *caller
	clone.Transport = &originTransport{base: caller.Transport, origin: origin, apiKey: apiKey}
	return &clone
}

// postJSON owns the common bounded HTTP exchange for text and face requests.
func (c *Client) postJSON(ctx context.Context, endpoint string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("embed request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, newAPIError(resp)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, &TransportError{Err: err}
	}
	if int64(len(payload)) > c.maxResponse {
		return nil, ErrResponseTooLarge
	}
	return payload, nil
}
