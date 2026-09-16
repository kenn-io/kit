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
		{"nullable prefix", "CHECK (error_code IS NULL OR error_code IN ('timeout', 'refused'))", "error_code"},
		{"nullable suffix", "CHECK (kind IN ('a', 'b') OR kind IS NULL)", "kind"},
		{"nested not in", "CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1 AND source NOT IN ('manual', 'import')))", "source"},
		{"in list beside other predicate", "CHECK (kind IN ('a') OR json_valid(payload))", "kind"},
		{"equality per branch", "CHECK ((state = 'running' AND started_at IS NOT NULL) OR (state = 'done' AND finished_at IS NOT NULL))", "state"},
		{"inequality chain", "CHECK (kind <> 'a' AND kind != 'b')", "kind"},
		{"postgres any array", "CHECK (status = ANY (ARRAY['queued', 'done']))", "status"},
		{"postgres any array cast", "CHECK (status = ANY (ARRAY['queued'::text, 'done'::text]))", "status"},
		{"postgres any literal array", "CHECK (status = ANY ('{queued,done}'::text[]))", "status"},
		{"not equal any", "CHECK (status <> ALL (ARRAY['x']))", "status"},
		{"lower wrapped nullable", "CHECK (role IS NULL OR lower(role) IN ('admin'))", "role"},
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
		"-- CHECK (kind IN ('a', 'b'))\nCREATE TABLE t (kind TEXT)",
		"/* CHECK (kind IN ('a')) */ CREATE TABLE t (kind TEXT)",
		"/* multi\n CHECK (kind IN ('a'))\n line */",
		"INSERT INTO notes (body) VALUES ('CHECK (kind IN (''a''))')",
		"-- CREATE TYPE s AS ENUM ('x')",
		"CREATE TABLE t (kind TEXT) -- was: CHECK (kind IN ('a'))",
		"CHECK (started_at IS NULL OR finished_at IS NULL OR started_at <= finished_at)",
		"CHECK ((state = 'done' AND finished_at IS NOT NULL) OR (state <> 'done' AND finished_at IS NULL))",
		"CHECK (SUBSTR(local_date, 6, 2) IN ('01', '02', '03'))",
		"CHECK (length(local_date) = 10 AND SUBSTR(local_date, 5, 1) = '-' AND SUBSTR(local_date, 8, 1) = '-')",
		"CHECK (retries = 0 OR max_retries = 0)",
		"CHECK (a = 1 AND b = 1)",
		"CHECK (name IN (SELECT name FROM allowed) OR name IN (SELECT alias FROM aliases))",
		"CHECK (started_at IN (SELECT 'x'))",
		"",
	} {
		assert.Empty(Scan(sql), sql)
	}
}

func TestScanReportsPositions(t *testing.T) {
	assert := assert.New(t)
	src := "CREATE TABLE t ( -- CHECK (id IN (1))\n  id INTEGER, /* 'CHECK' */\n  status TEXT CHECK (status IN ('a')),\n  other TEXT CHECK (other IN ('b'))\n);"
	findings := Scan(src)
	require.Len(t, findings, 2)
	assert.Equal(3, findings[0].Line)
	assert.Equal(15, findings[0].Column)
	assert.Equal(4, findings[1].Line)
	assert.Equal("other", findings[1].ColumnName)
}

func TestScanReportsEnumTypes(t *testing.T) {
	assert := assert.New(t)
	src := "CREATE TYPE job_status AS ENUM ('queued', 'done');\nCREATE TYPE \"public\".\"kind\" AS\n  ENUM ('a');\nCREATE TABLE t (id INT);"
	findings := Scan(src)
	require.Len(t, findings, 2)
	assert.Equal(EnumType, findings[0].Kind)
	assert.Equal("job_status", findings[0].ColumnName)
	assert.Equal("'queued', 'done'", findings[0].Expr)
	assert.Equal(1, findings[0].Line)
	assert.Equal(`"public"."kind"`, findings[1].ColumnName)
	assert.Equal(2, findings[1].Line)
	assert.Contains(findings[0].Message(), "enum type job_status")
	assert.Empty(Scan("CREATE TYPE money AS (amount INT, currency TEXT); CREATE TYPE t AS RANGE (subtype = int)"))
}

func TestScanOrdersFindingsByOffset(t *testing.T) {
	src := "CREATE TABLE t (kind TEXT CHECK (kind IN ('a')));\nCREATE TYPE s AS ENUM ('x');\nCREATE TABLE u (kind TEXT CHECK (kind IN ('b')));"
	findings := Scan(src)
	require.Len(t, findings, 3)
	assert.Equal(t, []Kind{CheckConstraint, EnumType, CheckConstraint}, []Kind{findings[0].Kind, findings[1].Kind, findings[2].Kind})
}

func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "a")
}
