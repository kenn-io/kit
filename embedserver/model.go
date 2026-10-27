package embedserver

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	cached, err := cacheModel(ctx, dir)
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

func normalize(vector []float32) {
	var squared float64
	for _, v := range vector {
		squared += float64(v) * float64(v)
	}
	if squared > 0 {
		scale := math.Sqrt(squared)
		for i := range vector {
			vector[i] = float32(float64(vector[i]) / scale)
		}
	}
}
