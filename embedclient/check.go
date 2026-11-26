package embedclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

// CheckOptions describes the service and model to check.
type CheckOptions struct {
	BaseURL, Model, APIKey string
	Dimensions             int
	Media                  []Input
}

func isUnsupported(err error) bool { return errors.Is(err, embedmodel.ErrUnsupportedContent) }

// Check checks the HTTP contract. Vector comparisons allow 1e-5 absolute plus
// 1e-5 relative error per component. It cannot judge media vector quality.
func Check(ctx context.Context, opts CheckOptions) []error {
	var failures []error
	fail := func(rule string, err any) {
		if e, ok := err.(error); ok {
			if contractErr, ok := errors.AsType[*checkContractError](e); ok {
				err = contractErr
			}
		}
		failures = append(failures, fmt.Errorf("%s: %v", rule, err))
	}
	model := embedconfig.Model{Name: opts.Model, Dimensions: opts.Dimensions, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone, RequestDimensions: true}
	makeClient := func(format string) (*Client, error) {
		m := model
		m.EncodingFormat = format
		c, err := New(Options{Model: m, Deployment: embedconfig.Deployment{API: embedconfig.APIKit, BaseURL: opts.BaseURL, TrustPrivateNetwork: true}, APIKey: opts.APIKey})
		if err != nil {
			return nil, err
		}
		c.http.Transport = &checkTransport{base: c.http.Transport, maxResponse: c.maxResponse}
		return c, nil
	}
	client, err := makeClient("float")
	if err != nil {
		fail("configuration", err)
		return failures
	}
	for _, format := range []string{"float", "base64"} {
		if err := checkWireEncoding(ctx, client, opts, format); err != nil {
			fail(format+" encoding", err)
		}
	}
	inputs := []Input{{Role: embedconfig.RoleQuery, Text: "alpha"}, {Role: embedconfig.RoleDocument, Text: "beta"}}
	batch, err := client.Encode(ctx, opts.Dimensions, inputs)
	if err != nil {
		fail("text batch", err)
	} else {
		for i := range inputs {
			one, e := client.Encode(ctx, opts.Dimensions, inputs[i:i+1])
			if e != nil {
				fail("singleton", e)
			} else if !closeVector(batch[i], one[0]) {
				fail("batch consistency", "singleton differs")
			}
		}
		encoded, e := makeClient("base64")
		if e != nil {
			fail("base64 configuration", e)
		} else if vectors, e := encoded.Encode(ctx, opts.Dimensions, inputs); e != nil {
			fail("base64", e)
		} else if !closeVectors(batch, vectors) {
			fail("base64 consistency", "vectors differ")
		}
	}
	fixtures := opts.Media
	optional := len(fixtures) == 0
	if optional {
		png, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC")
		fixtures = []Input{{Role: embedconfig.RoleDocument, MediaType: "image/png", Data: png}}
	}
	for _, f := range fixtures {
		_, e := client.Encode(ctx, opts.Dimensions, []Input{f})
		if e != nil && (!optional || !isUnsupported(e)) {
			fail("media", e)
		}
	}
	type badCase struct {
		name, method, body string
		status             int
		code               string
	}
	cases := []badCase{
		{"unknown model", "POST", fmt.Sprintf(`{"model":%q,"inputs":[{"role":"query","text":"a"}]}`, opts.Model+"-unknown"), 404, "model_not_found"},
		{"malformed", "POST", "{", 400, "invalid_request"},
		{"empty inputs", "POST", fmt.Sprintf(`{"model":%q,"inputs":[]}`, opts.Model), 400, "invalid_request"},
		{"bad role", "POST", fmt.Sprintf(`{"model":%q,"inputs":[{"role":"wrong","text":"a"}]}`, opts.Model), 400, "invalid_request"},
		{"bad base64", "POST", fmt.Sprintf(`{"model":%q,"inputs":[{"role":"query","media_type":"image/png","data":"!"}]}`, opts.Model), 400, "invalid_request"},
		{"both payloads", "POST", fmt.Sprintf(`{"model":%q,"inputs":[{"role":"query","text":"a","media_type":"image/png","data":"AQ=="}]}`, opts.Model), 400, "invalid_request"},
		{"missing payload", "POST", fmt.Sprintf(`{"model":%q,"inputs":[{"role":"query"}]}`, opts.Model), 400, "invalid_request"},
		{"missing media data", "POST", fmt.Sprintf(`{"model":%q,"inputs":[{"role":"query","media_type":"image/png"}]}`, opts.Model), 400, "invalid_request"},
		{"method", "GET", "", 405, "method_not_allowed"},
	}
	for _, tc := range cases {
		req, e := http.NewRequestWithContext(ctx, tc.method, strings.TrimRight(opts.BaseURL, "/")+"/embed", bytes.NewBufferString(tc.body))
		if e != nil {
			fail(tc.name, e)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if opts.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+opts.APIKey)
		}
		resp, e := client.http.Do(req)
		if e != nil {
			fail(tc.name, e)
			continue
		}
		body, e := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if e != nil {
			fail(tc.name, e)
			continue
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		e = json.Unmarshal(body, &envelope)
		if e != nil || resp.StatusCode != tc.status || envelope.Error.Code != tc.code {
			fail(tc.name, fmt.Sprintf("got status %d and code %q", resp.StatusCode, envelope.Error.Code))
		}
		if tc.status == 405 && resp.Header.Get("Allow") != "POST" {
			fail(tc.name, "Allow must be POST")
		}
	}
	return failures
}

func checkWireEncoding(ctx context.Context, c *Client, opts CheckOptions, format string) error {
	body, err := json.Marshal(struct {
		Model      string  `json:"model"`
		Dimensions int     `json:"dimensions"`
		Encoding   string  `json:"encoding_format"`
		Inputs     []Input `json:"inputs"`
	}{opts.Model, opts.Dimensions, format, []Input{{Role: embedconfig.RoleQuery, Text: "alpha"}, {Role: embedconfig.RoleDocument, Text: "beta"}}})
	if err != nil {
		return err
	}
	payload, err := c.postJSON(ctx, c.endpoint, body)
	if err != nil {
		return err
	}
	var response struct {
		Data []struct {
			Embedding json.RawMessage `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return err
	}
	if len(response.Data) != 2 {
		return errors.New("expected two embeddings")
	}

	want := byte('[')
	if format == "base64" {
		want = '"'
	}
	for _, item := range response.Data {
		raw := bytes.TrimSpace(item.Embedding)
		if len(raw) == 0 || raw[0] != want {
			return fmt.Errorf("expected %s embedding", format)
		}
	}
	if _, problems, err := c.classify(payload, 2); err != nil {
		return err
	} else if len(problems) > 0 {
		return problems[0]
	}
	return nil
}

type checkContractError struct{ message string }

func (e *checkContractError) Error() string { return e.message }

// checkTransport enforces the Kit response contract on every successful request.
type checkTransport struct {
	base        http.RoundTripper
	maxResponse int64
}

func (t *checkTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &checkContractError{message: fmt.Sprintf("expected status 200, got %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, t.maxResponse+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > t.maxResponse {
		return nil, ErrResponseTooLarge
	}
	var result struct {
		Data []struct {
			Index *int `json:"index"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	seen := make([]bool, len(result.Data))
	for _, item := range result.Data {
		if item.Index == nil || *item.Index < 0 || *item.Index >= len(seen) || seen[*item.Index] {
			return nil, &checkContractError{message: "response indexes must be present, unique, and in range"}
		}
		seen[*item.Index] = true
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func closeVector(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i, x := range a {
		y := b[i]
		if math.Abs(float64(x)-float64(y)) > 1e-5+1e-5*math.Max(math.Abs(float64(x)), math.Abs(float64(y))) {
			return false
		}
	}
	return true
}

func closeVectors(a, b [][]float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !closeVector(a[i], b[i]) {
			return false
		}
	}
	return true
}
