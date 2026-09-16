package sqlenum

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/analysis/analysistest"
)

func TestScanReportsEnumConstraints(t *testing.T) {
	tests := []struct {
		name   string
		sql    string
		column string
	}{
		{"in list", `status TEXT NOT NULL CHECK(status IN ('queued','running','done')) DEFAULT 'queued'`, "status"},
		{"spaced in list", "CHECK (subject_kind IN ('pull_request', 'issue', 'workspace'))", "subject_kind"},
		{"not in", "CHECK (kind NOT IN ('a', 'b'))", "kind"},
		{"integers", "CHECK (level IN (0, 1, 2))", "level"},
		{"or chain", "CHECK (mode = 'fast' OR mode = 'slow')", "mode"},
		{"quoted column", `CHECK ("state" IN ('x'))`, `"state"`},
		{"lower wrapped", "CHECK (lower(role) IN ('admin', 'member'))", "role"},
		{"multiline", "CONSTRAINT valid_kind\n    CHECK (\n      kind IN (\n        'one',\n        'two'\n      )\n    )", "kind"},
		{"escaped quote", "CHECK (name IN ('it''s', 'ok'))", "name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := Scan(tc.sql)
			require.Len(t, findings, 1, "expected one finding")
			assert.Equal(t, tc.column, findings[0].ColumnName)
			assert.Contains(t, findings[0].Message(), tc.column)
		})
	}
}

func TestScanIgnoresOtherConstraints(t *testing.T) {
	assert := assert.New(t)
	for _, sql := range []string{
		"CHECK (length(name) > 0)",
		"CHECK (started_at <= finished_at)",
		"CHECK (retries >= 0 AND retries <= 10)",
		"CHECK (status IN (SELECT name FROM statuses))",
		"CHECK (json_valid(payload))",
		"-- CHECK the docs",
		"CHECK (kind IN ('a') OR json_valid(payload))",
		"",
	} {
		assert.Empty(Scan(sql), sql)
	}
}

func TestScanReportsPositions(t *testing.T) {
	assert := assert.New(t)
	src := "CREATE TABLE t (\n  id INTEGER,\n  status TEXT CHECK (status IN ('a')),\n  other TEXT CHECK (other IN ('b'))\n);"
	findings := Scan(src)
	require.Len(t, findings, 2)
	assert.Equal(3, findings[0].Line)
	assert.Equal(15, findings[0].Column)
	assert.Equal(4, findings[1].Line)
	assert.Equal("other", findings[1].ColumnName)
}

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a")
}
