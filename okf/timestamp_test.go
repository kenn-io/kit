package okf_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/okf"
)

func TestTimestamp(t *testing.T) {
	for _, value := range []string{"2026-11-17", "'2026-11-17'", "2026-11-17T13:00:00+01:00", "\"2026-11-17t12:00:00z\"", "\"2026-11-17T07:00:00-05:00\""} {
		t.Run(value, func(t *testing.T) {
			b, err := okf.Parse([]byte("---\ngenerated: {at: " + value + "}\n---\n"))
			require.NoError(t, err)

			got, ok := okf.Timestamp(b.Field("generated", "at"))
			require.True(t, ok)
			want := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			if strings.Contains(strings.ToUpper(value), "T") {
				assert.True(t, got.Equal(want))
			} else {
				assert.Equal(t, want.Add(-12*time.Hour), got)
			}
		})
	}
}

func TestTimestampSyntax(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"2026-11-17T1:00:00Z", false},
		{"2026-11-17T12:00:00,25Z", false},
		{"2026-11-17T12:00:00+24:00", false},
		{"2026-11-17T12:00:00-24:00", false},
		{"2026-11-17T12:00:00+01:60", false},
		{"2026-11-17T12:00:00-01:60", false},
		{"2026-11-17T12:00:00+23:59", true},
		{"2026-11-17T12:00:00-23:59", true},
		{"2026-11-17T12:00:00+00:59", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			src := "---\ngenerated: {at: '" + tc.value + "'}\n---\n"
			b, err := okf.Parse([]byte(src))
			require.NoError(t, err)
			_, ok := okf.Timestamp(b.Field("generated", "at"))
			assert.Equal(t, tc.valid, ok)
		})
	}
}
