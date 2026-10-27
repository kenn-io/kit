package embedserver

import (
	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
)

func loadTokenizer(path string) (*tokenizer.Tokenizer, error) {
	t, err := pretrained.FromFile(path)
	if err != nil {
		return nil, err
	}
	t.WithTruncation(&tokenizer.TruncationParams{MaxLength: 2048, Strategy: tokenizer.LongestFirst})
	return t, nil
}
