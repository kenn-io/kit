package okf_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/okf"
)

func TestTimestamp(t *testing.T) {
	noon := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Time
		valid bool
	}{
		{"2026-11-17", noon.Add(-12 * time.Hour), true},
		{"'2026-11-17'", noon.Add(-12 * time.Hour), true},
		{"2026-11-17T13:00:00+01:00", noon, true},
		{"\"2026-11-17T07:00:00-05:00\"", noon, true},
		{"'2026-11-17T12:00:00'", time.Time{}, false},
		{"17 November 2026", time.Time{}, false},
		{"42", time.Time{}, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			doc, err := okf.Parse([]byte("---\ngenerated: {at: " + tc.value + "}\n---\n"))
			require.NoError(t, err)
			got, ok := okf.Timestamp(doc.Field("generated", "at"))
			assert.Equal(t, tc.valid, ok)
			if tc.valid {
				assert.True(t, got.Equal(tc.want), got)
			}
		})
	}
}
