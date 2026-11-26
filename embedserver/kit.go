package embedserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	jsonv1 "encoding/json"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
)

// Local runs the built-in text model without starting an HTTP service.
type Local struct{ h *handler }

// NewLocal opens the model engine. The caller must close it after calls finish.
func NewLocal(ctx context.Context, dir, library string) (*Local, error) {
	e, err := loadModels(ctx, dir, library)
	if err != nil {
		return nil, err
	}
	return newLocal(e), nil
}

func newLocal(e engine) *Local {
	return &Local{h: &handler{engine: e, gate: make(chan struct{}, 1), previews: make(chan struct{}, 1), log: io.Discard}}
}

// Close releases the model engine after active calls finish.
func (l *Local) Close() error {
	l.h.gate <- struct{}{}
	defer func() { <-l.h.gate }()
	return l.h.engine.Close()
}

// Encode encodes text as given. The built-in model does not support media.
func (l *Local) Encode(ctx context.Context, dims int, inputs []embedclient.Input) ([][]float32, error) {
	if dims == 0 {
		dims = 768
	}
	if dims < 1 || dims > 768 {
		return nil, embedclient.ErrInvalidDimensions
	}
	out := make([][]float32, len(inputs))
	for i, input := range inputs {
		if err := input.Validate(); err != nil {
			return nil, err
		}
		if input.MediaType != "" {
			return nil, embedmodel.ErrUnsupportedContent
		}
		v, _, err := l.h.embed(ctx, input.Text)
		if err != nil {
			return nil, err
		}
		out[i] = normalizeDimensions(v, dims)
	}
	return out, nil
}

func normalizeDimensions(v []float32, dims int) []float32 {
	v = v[:dims]
	var squared float64
	for _, x := range v {
		squared += float64(x) * float64(x)
	}
	if norm := math.Sqrt(squared); norm > 0 {
		for i, x := range v {
			v[i] = float32(float64(x) / norm)
		}
	}
	return v
}

func encodeEmbedding(v []float32, format string) any {
	if format != "base64" {
		return v
	}
	data := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(data[i*4:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(data)
}

// Handler serves the Kit embedding HTTP contract for the named models.
func Handler(backend embedclient.Backend, models []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openai := strings.HasSuffix(r.URL.Path, "/embeddings")
		if !openai && !strings.HasSuffix(r.URL.Path, "/embed") {
			http.NotFound(w, r)
			return
		}
		fail := func(status int, code, message string) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.MarshalWrite(w, map[string]any{"error": map[string]string{"code": code, "message": message}})
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			fail(405, "method_not_allowed", "POST required")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				fail(413, "input_too_long", "request exceeds 64 MiB")
			} else {
				fail(400, "invalid_request", "could not read body")
			}
			return
		}
		var req struct {
			Model      string              `json:"model"`
			Dimensions int                 `json:"dimensions"`
			Encoding   string              `json:"encoding_format"`
			Inputs     []embedclient.Input `json:"inputs"`
			Input      jsonv1.RawMessage   `json:"input"`
			InputType  string              `json:"input_type"`
		}
		if err := json.Unmarshal(body, &req, decodeOptions); err != nil {
			fail(400, "invalid_request", "invalid request")
			return
		}
		if openai {
			req.Inputs = nil
			var texts []string
			var text *string
			if json.Unmarshal(req.Input, &text, decodeOptions) == nil && text != nil {
				texts = []string{*text}
			} else {
				var items []*string
				if err := json.Unmarshal(req.Input, &items, decodeOptions); err != nil {
					fail(400, "invalid_request", "invalid input")
					return
				}
				for _, item := range items {
					if item == nil {
						fail(400, "invalid_request", "invalid input")
						return
					}
					texts = append(texts, *item)
				}
			}
			role := embedconfig.RoleDocument
			if req.InputType == "query" {
				role = embedconfig.RoleQuery
			}
			for _, text := range texts {
				req.Inputs = append(req.Inputs, embedclient.Input{Role: role, Text: text})
			}
		}
		if req.Model == "" || len(req.Inputs) == 0 || req.Dimensions < 0 || (req.Encoding != "" && req.Encoding != "float" && req.Encoding != "base64") {
			fail(400, "invalid_request", "invalid request")
			return
		}
		found := false
		for _, name := range models {
			if req.Model == name {
				found = true
				break
			}
		}
		if !found {
			fail(404, "model_not_found", "model not found")
			return
		}
		for _, in := range req.Inputs {
			if in.Validate() != nil {
				fail(400, "invalid_request", "invalid input")
				return
			}
		}
		vectors, err := backend.Encode(r.Context(), req.Dimensions, req.Inputs)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			if errors.Is(err, embedclient.ErrInvalidDimensions) {
				fail(400, "invalid_request", "invalid dimensions")
				return
			}
			if errors.Is(err, embedmodel.ErrUnsupportedContent) {
				fail(415, "unsupported_content", "content is unsupported")
				return
			}
			var load *modelLoadError
			if errors.As(err, &load) {
				if local, ok := backend.(*Local); ok {
					_, _ = fmt.Fprintf(local.h.log, "inference failed: %v\n", err)
				}
				fail(424, "model_unavailable", "model could not be loaded")
				return
			}
			var api *embedclient.APIError
			if errors.As(err, &api) {
				switch {
				case api.Reason == embedclient.ReasonInputTooLong:
					fail(413, "input_too_long", "input too long")
					return
				case api.Reason == embedclient.ReasonContentPolicy:
					fail(400, "content_policy_violation", "content refused by policy")
					return
				case api.CredentialsRejected():
					fail(502, "upstream_credentials", "upstream credentials rejected")
					return
				case api.StatusCode == http.StatusTooManyRequests:
					if api.RetryAfter > 0 {
						w.Header().Set("Retry-After", strconv.FormatInt(int64((api.RetryAfter+time.Second-1)/time.Second), 10))
					}
					fail(429, "rate_limited", "upstream rate limited")
					return
				case api.Reason == embedclient.ReasonInvalidRequest:
					fail(400, "invalid_request", "upstream request invalid")
					return
				}
			}
			fail(503, "unavailable", "embedding failed")
			return
		}
		if len(vectors) != len(req.Inputs) {
			fail(500, "internal_error", "invalid backend response")
			return
		}
		for _, v := range vectors {
			if len(v) == 0 || (req.Dimensions > 0 && len(v) != req.Dimensions) {
				fail(500, "internal_error", "invalid backend response")
				return
			}
			nonzero := false
			for _, x := range v {
				if x != 0 {
					nonzero = true
				}
				if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
					fail(500, "internal_error", "invalid backend response")
					return
				}
			}
			if !nonzero {
				fail(500, "internal_error", "invalid backend response")
				return
			}
		}
		data := make([]item, len(vectors))
		for i, v := range vectors {
			data[i] = item{Index: i, Embedding: encodeEmbedding(v, req.Encoding)}
			if openai {
				data[i].Object = "embedding"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{"model": req.Model, "data": data}
		if openai {
			out["object"] = "list"
		}
		_ = json.MarshalWrite(w, out)
	})
}
