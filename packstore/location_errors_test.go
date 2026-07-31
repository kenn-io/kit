package packstore

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestClassifyIntegrityError(t *testing.T) {
	tests := []struct {
		name    string
		input   error
		corrupt bool
	}{
		{name: "content mismatch", input: ErrContentMismatch, corrupt: true},
		{name: "bad magic", input: pack.ErrBadMagic, corrupt: true},
		{name: "unsupported version", input: pack.ErrUnsupportedVersion, corrupt: true},
		{name: "truncated", input: pack.ErrTruncated, corrupt: true},
		{name: "checksum", input: pack.ErrChecksum, corrupt: true},
		{name: "corrupt", input: pack.ErrCorrupt, corrupt: true},
		{name: "blob mismatch", input: pack.ErrBlobMismatch, corrupt: true},
		{name: "verification incomplete", input: pack.ErrVerificationIncomplete},
		{name: "context canceled", input: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ClassifyIntegrityError(tt.input)

			assert.Equal(t, tt.corrupt, errors.Is(err, ErrPhysicalCorrupt))
			require.ErrorIs(t, err, tt.input)
		})
	}
}
