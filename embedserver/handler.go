package embedserver

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
)

type handler struct {
	engine engine
	gate   chan struct{}
}
type request struct {
	Model      string          `json:"model"`
	Input      json.RawMessage `json:"input"`
	Dimensions int             `json:"dimensions"`
	Encoding   string          `json:"encoding_format"`
}
type item struct {
	Object    string `json:"object"`
	Index     int    `json:"index"`
	Embedding any    `json:"embedding"`
}
type usage struct {
	Prompt int `json:"prompt_tokens"`
	Total  int `json:"total_tokens"`
}
type response struct {
	Object string `json:"object"`
	Data   []item `json:"data"`
	Model  string `json:"model"`
	Usage  usage  `json:"usage"`
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "request exceeds 4 MiB", http.StatusRequestEntityTooLarge)
		return
	}
	var req request
	if err := jsonv2.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Model != "nomic-embed-text" && req.Model != "nomic-embed-text:latest" {
		http.Error(w, "model not found", http.StatusNotFound)
		return
	}
	var inputs []string
	var text *string
	if jsonv2.Unmarshal(req.Input, &text) == nil && text != nil {
		inputs = []string{*text}
	} else {
		var texts []*string
		if err := jsonv2.Unmarshal(req.Input, &texts); err != nil {
			http.Error(w, "input must be a string or string array", http.StatusBadRequest)
			return
		}
		for _, text := range texts {
			if text == nil {
				http.Error(w, "input must contain strings", http.StatusBadRequest)
				return
			}
			inputs = append(inputs, *text)
		}
	}
	if len(inputs) == 0 || len(inputs) > 32 || req.Dimensions > 768 || (req.Encoding != "" && req.Encoding != "float" && req.Encoding != "base64") {
		http.Error(w, "invalid input count, dimensions, or encoding_format", http.StatusBadRequest)
		return
	}
	if req.Dimensions <= 0 {
		req.Dimensions = 768
	}
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	case <-r.Context().Done():
		return
	}
	out, err := h.embeddings(r.Context(), req, inputs)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		http.Error(w, "inference failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = jsonv2.MarshalWrite(w, out)
}

func (h *handler) embeddings(ctx context.Context, req request, inputs []string) (response, error) {
	out := response{Object: "list", Model: req.Model, Data: make([]item, len(inputs))}
	for i, text := range inputs {
		vector, tokens, err := h.engine.embed(ctx, text)
		if err != nil {
			return out, err
		}
		vector = vector[:req.Dimensions]
		normalize(vector)
		var embedding any = vector
		if req.Encoding == "base64" {
			data := make([]byte, len(vector)*4)
			for j, v := range vector {
				binary.LittleEndian.PutUint32(data[j*4:], math.Float32bits(v))
			}
			embedding = base64.StdEncoding.EncodeToString(data)
		}
		out.Data[i] = item{Object: "embedding", Index: i, Embedding: embedding}
		out.Usage.Prompt += tokens
		out.Usage.Total += tokens
	}
	return out, nil
}
