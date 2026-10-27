package embedserver

import (
	"context"
	"errors"
	"math"
	"path/filepath"

	"github.com/sugarme/tokenizer"
)

type engine interface {
	embed(context.Context, string) ([]float32, int, error)
	Close() error
}
type model struct {
	ort       *ort
	tokenizer *tokenizer.Tokenizer
}

func loadModel(ctx context.Context, dir, library string) (engine, error) {
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
	encoded, err := m.tokenizer.EncodeSingle(text, true)
	if err != nil {
		return nil, 0, err
	}
	if len(encoded.Ids) == 0 {
		return nil, 0, errors.New("tokenizer returned no tokens")
	}
	vector, err := m.ort.infer(ctx, encoded.Ids, encoded.AttentionMask, encoded.TypeIds)
	return vector, len(encoded.Ids), err
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
