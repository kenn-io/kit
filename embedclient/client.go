package embedclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	jsonv2 "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

// Options configures a text embedding client.
type Options struct {
	Model      embedconfig.Model
	Roles      embedconfig.Roles
	Deployment embedconfig.Deployment
	Batch      embedconfig.Batch
	Transport  embedconfig.Transport
	// APIKey is the resolved bearer token. It is not part of any identity.
	// Empty means the request does not add an Authorization header.
	APIKey string
	// HTTP is an optional caller-owned client. New clones it and leaves the
	// original unchanged. Nil builds a client from Transport.Timeout.
	HTTP *http.Client
}

// Client sends text embedding requests.
type Client struct {
	http        *http.Client
	endpoint    string
	model       embedconfig.Model
	roles       embedconfig.Roles
	batchItems  int
	maxResponse int64
}

// New validates opts and returns a client pinned to the deployment origin.
func New(opts Options) (*Client, error) {
	model, err := opts.Model.Prepared()
	if err != nil {
		return nil, err
	}
	roles, err := opts.Roles.Prepared()
	if err != nil {
		return nil, err
	}
	deployment, err := opts.Deployment.Prepared()
	if err != nil {
		return nil, err
	}
	batch, err := opts.Batch.Prepared()
	if err != nil {
		return nil, err
	}
	transport, err := opts.Transport.Prepared()
	if err != nil {
		return nil, err
	}
	canonical, err := deployment.Canonical()
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(canonical)
	if err != nil {
		return nil, fmt.Errorf("embed endpoint is invalid: %w", err)
	}
	origin, err := embedconfig.Origin(parsed)
	if err != nil {
		return nil, err
	}
	return &Client{
		http:        pinClient(opts.HTTP, origin, opts.APIKey, transport.Timeout),
		endpoint:    embeddingsURL(canonical),
		model:       model,
		roles:       roles,
		batchItems:  batch.Items,
		maxResponse: int64(transport.MaxResponseBytes),
	}, nil
}

// Embed encodes inputs in request order. An empty list returns nil.
// Inputs that do not share a role are sent in separate requests because
// input_type is one field per request.
func (c *Client) Embed(ctx context.Context, inputs []embedmodel.Content) ([][]float32, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	prepared, err := c.prepare(inputs)
	if err != nil {
		return nil, err
	}
	out := make([][]float32, len(prepared))
	for start := 0; start < len(prepared); {
		end := start + 1
		for end < len(prepared) && prepared[end].role == prepared[start].role && end-start < c.batchItems {
			end++
		}
		texts := make([]string, end-start)
		for i := start; i < end; i++ {
			texts[i-start] = prepared[i].text
		}
		vectors, err := c.post(ctx, prepared[start].role, texts)
		if err != nil {
			return nil, err
		}
		for i, vec := range vectors {
			out[prepared[start+i].index] = vec
		}
		start = end
	}
	return out, nil
}

// EncodeFunc adapts one role to vector.EncodeFunc.
func (c *Client) EncodeFunc(role embedconfig.Role) vector.EncodeFunc {
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		inputs := make([]embedmodel.Content, len(texts))
		for i, text := range texts {
			inputs[i] = embedmodel.Content{Role: role, Kind: embedmodel.KindText, Text: text}
		}
		return c.Embed(ctx, inputs)
	}
}

type encoded struct {
	index int
	role  embedconfig.Role
	text  string
}

func (c *Client) prepare(inputs []embedmodel.Content) ([]encoded, error) {
	out := make([]encoded, len(inputs))
	for i, input := range inputs {
		text, err := input.EmbedText()
		if err != nil {
			return nil, fmt.Errorf("embed input %d: %w", i, err)
		}
		if embedmodel.BlankText(text) {
			return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
		}
		formatted, err := embedmodel.Format(input.Role, text, c.roles)
		if err != nil {
			return nil, fmt.Errorf("embed input %d: %w", i, err)
		}
		if embedmodel.BlankText(formatted) {
			return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
		}
		out[i] = encoded{index: i, role: input.Role, text: formatted}
	}
	return out, nil
}

func (c *Client) post(ctx context.Context, role embedconfig.Role, texts []string) ([][]float32, error) {
	body, err := c.requestBody(role, texts)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("embed request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, classifyTransport(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			RetryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, errors.New("embed response is invalid")
	}
	if int64(len(payload)) > c.maxResponse {
		return nil, errors.New("embed response exceeds the configured cap")
	}
	return c.decode(payload, len(texts))
}

func embeddingsURL(canonical string) string {
	if strings.HasSuffix(canonical, "/embeddings") {
		return canonical
	}
	return canonical + "/embeddings"
}

// requestBody is split so the wire struct stays next to the encoder.
func (c *Client) requestBody(role embedconfig.Role, texts []string) ([]byte, error) {
	payload := wireRequest{Model: c.model.Name, Input: texts}
	if c.roles.InputType == embedconfig.InputTypeRetrieval {
		payload.InputType = string(role)
	}
	if c.model.RequestDimensions {
		payload.Dimensions = c.model.Dimensions
	}
	if c.model.EncodingFormat != "" {
		payload.EncodingFormat = c.model.EncodingFormat
	}
	body, err := jsonv2.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("embed request encoding failed: %w", err)
	}
	return body, nil
}
