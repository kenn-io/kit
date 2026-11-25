package embedserver

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/internal/embedassets"
)

type engine interface {
	embed(context.Context, string) ([]float32, int, error)
	Close() error
}
type model struct {
	ort       *ort
	tokenizer *tokenizer
}

func loadModel(ctx context.Context, dir, library string) (engine, error) {
	library, err := filepath.Abs(library)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(library); err != nil {
		return nil, fmt.Errorf("ONNX Runtime library: %w", err)
	}
	cached, err := embedassets.InstalledModel(dir, embedassets.TextModel)
	if err != nil {
		return nil, err
	}
	t, err := loadTokenizer(filepath.Join(cached, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	o, err := openORT(library, filepath.Join(cached, "model.onnx"))
	if err != nil {
		return nil, err
	}
	return &model{o, t}, nil
}
func (m *model) Close() error { return m.ort.Close() }
func (m *model) embed(ctx context.Context, text string) ([]float32, int, error) {
	ids := m.tokenizer.encode(text)
	vector, err := m.ort.infer(ctx, ids)
	return vector, len(ids), err
}

// embedStrict rejects text beyond the model window before inference.
func (m *model) embedStrict(ctx context.Context, text string) ([]float32, int, error) {
	ids := m.tokenizer.encodeLimit(text, 2049)
	if len(ids) > 2048 {
		return nil, 0, &embedclient.APIError{StatusCode: http.StatusRequestEntityTooLarge, Reason: embedclient.ReasonInputTooLong}
	}
	v, err := m.ort.infer(ctx, ids)
	return v, len(ids), err
}
