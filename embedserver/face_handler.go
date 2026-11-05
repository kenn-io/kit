package embedserver

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/internal/embedassets"
)

func (h *handler) faces(w http.ResponseWriter, r *http.Request) {
	var req embedclient.FaceRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		http.Error(w, "request exceeds 64 MiB or could not be read", http.StatusRequestEntityTooLarge)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid face request", http.StatusBadRequest)
		return
	}
	if len(req.Boxes) == 0 || len(req.Boxes) > embedclient.MaxFaceBoxes {
		http.Error(w, "invalid face box count", http.StatusBadRequest)
		return
	}
	for _, box := range req.Boxes {
		if err := box.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(req.Image))
	if err != nil || config.Width <= 0 || config.Height <= 0 || config.Width > embedclient.MaxPreviewPixels/config.Height {
		http.Error(w, "invalid preview or too many pixels", http.StatusBadRequest)
		return
	}
	// Hold the shared gate through decoding and all boxes so only one decoded
	// preview is retained at a time. Text requests wait until this photo finishes.
	select {
	case h.gate <- struct{}{}:
	case <-r.Context().Done():
		return
	}
	defer func() { <-h.gate }()
	preview, _, err := image.Decode(bytes.NewReader(req.Image))
	if err != nil {
		http.Error(w, "invalid preview", http.StatusBadRequest)
		return
	}
	result := embedclient.FaceResponse{Model: embedclient.FaceModel, Revision: embedclient.FaceRevision, Recipe: embedclient.FaceRecipe, Data: make([]embedclient.FaceResult, len(req.Boxes))}
	for i, box := range req.Boxes {
		vector, err := h.embedFace(r.Context(), preview, box)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return
			}
			_, _ = fmt.Fprintf(h.log, "face inference failed: %v\n", err)
			if load, ok := errors.AsType[*modelLoadError](err); ok {
				http.Error(w, load.Error(), http.StatusFailedDependency)
			} else {
				http.Error(w, "face inference failed", http.StatusInternalServerError)
			}
			return
		}
		result.Data[i] = embedclient.FaceResult{Index: i, Embedding: vector}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, result, json.FormatNilSliceAsNull(true)); err != nil {
		_, _ = fmt.Fprintf(h.log, "encode faces: %v\n", err)
	}
}

func (h *handler) embedFace(ctx context.Context, preview image.Image, box embedclient.FaceBox) ([]float32, error) {
	m, ok := h.engine.(interface {
		embedFace(context.Context, image.Image, embedclient.FaceBox) ([]float32, error)
	})
	if !ok {
		return nil, errors.New("face models unavailable")
	}
	return m.embedFace(ctx, preview, box)
}

func (h *handler) availableModels(w http.ResponseWriter, r *http.Request) {
	available := []string{}
	if m, ok := h.engine.(*models); ok {
		for _, name := range []string{embedassets.TextModel, embedassets.FaceModel} {
			if embedassets.ModelAvailable(m.dir, name) {
				available = append(available, name)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, struct {
		Models []string `json:"models"`
	}{available}); err != nil {
		_, _ = fmt.Fprintf(h.log, "encode models: %v\n", err)
	}
}
