package vector_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/kit/vector"
)

func TestGenerationFingerprintIsStableAndOrderIndependent(t *testing.T) {
	assert := assert.New(t)
	a := vector.Generation{
		Model:      "text-embedding-3-small",
		Dimensions: 1536,
		Params:     map[string]string{"pooling": "mean", "prompt": "search"},
	}
	b := vector.Generation{
		Model:      "text-embedding-3-small",
		Dimensions: 1536,
		Params:     map[string]string{"prompt": "search", "pooling": "mean"},
	}

	assert.Equal(a.Fingerprint(), a.Fingerprint(), "same value fingerprints identically")
	assert.Equal(a.Fingerprint(), b.Fingerprint(), "map order does not change fingerprint")
}

func TestGenerationFingerprintIsNotAmbiguousAcrossParams(t *testing.T) {
	assert := assert.New(t)
	// Two params vs a single param whose value embeds what used to be the
	// key/value separator. A naive "key=value\n" join hashes both the
	// same; the JSON encoding keeps them distinct.
	two := vector.Generation{Model: "m", Dimensions: 3, Params: map[string]string{"pooling": "mean", "prompt": "x"}}
	one := vector.Generation{Model: "m", Dimensions: 3, Params: map[string]string{"pooling": "mean\nprompt=x"}}

	assert.NotEqual(two.Fingerprint(), one.Fingerprint())
}

func TestGenerationFingerprintChangesWithSpace(t *testing.T) {
	assert := assert.New(t)
	base := vector.Generation{Model: "m", Dimensions: 768, Params: map[string]string{"pooling": "mean"}}

	cases := map[string]vector.Generation{
		"model":      {Model: "other", Dimensions: 768, Params: map[string]string{"pooling": "mean"}},
		"dimensions": {Model: "m", Dimensions: 1024, Params: map[string]string{"pooling": "mean"}},
		"param value": {Model: "m", Dimensions: 768, Params: map[string]string{"pooling": "cls"}},
		"extra param": {Model: "m", Dimensions: 768, Params: map[string]string{"pooling": "mean", "prompt": "x"}},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			assert.NotEqual(base.Fingerprint(), g.Fingerprint())
		})
	}
}
