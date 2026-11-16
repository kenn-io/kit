package embedserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIntraOpThreads(t *testing.T) {
	for _, tc := range []struct{ allowed, host, want int }{
		{8, 256, 8},
		{256, 256, 0},
		{16, 8, 0},
		{1, 2, 1},
	} {
		assert.Equal(t, tc.want, intraOpThreads(tc.allowed, tc.host))
	}
}
