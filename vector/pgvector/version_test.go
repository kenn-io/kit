package pgvector

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSupportedVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"0.7.4", false},
		{"0.8.0", true},
		{"0.10.0", true},
		{"1.0.0", true},
		{"", false},
		{"0.8", false},
		{"0.8.0.extra", false},
		{"0.8.0junk", false},
		{"-1.8.0", false},
		{"0..0", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			assert.Equal(t, tc.want, supportedVersion(tc.version))
		})
	}
}
