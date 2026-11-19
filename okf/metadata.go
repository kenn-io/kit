package okf

import (
	"strings"
	"time"
)

// OKF v0.3 §11.2: an empty or null value is an absent key, and a list field
// accepts one item as a scalar. List members are never dropped, and the
// values of x- extension keys are kept as written.
var (
	listFields = []string{"tags", "sources", "parameters", "verified"}
	// absentMappings are defined mapping fields whose empty form is also absent.
	absentMappings = map[string]bool{"usage_window": true, "executor": true, "attester": true}
)

// NormalizedMetadata returns a fresh mapping for semantic comparisons. It
// normalizes absent values, singleton lists and timestamps without editing the document.
func (d *Document) NormalizedMetadata() (map[string]any, error) {
	header := make(map[string]any)
	if err := d.Metadata.Decode(&header); err != nil {
		return nil, err
	}
	pruneAbsent(header)
	for _, key := range listFields {
		if value, ok := header[key]; ok {
			if _, isList := value.([]any); !isList {
				header[key] = []any{value}
			}
		}
	}
	canonicalTimestamps(header)
	return header, nil
}

func pruneAbsent(m map[string]any) {
	for key, value := range m {
		if !strings.HasPrefix(key, "x-") {
			pruneNested(value)
		}
		if emptyOptionalValue(value) || (absentMappings[key] && emptyMapping(value)) {
			delete(m, key)
		}
	}
}

func emptyMapping(value any) bool {
	m, ok := value.(map[string]any)
	return ok && len(m) == 0
}

func pruneNested(value any) {
	switch v := value.(type) {
	case map[string]any:
		pruneAbsent(v)
	case []any:
		for _, item := range v {
			pruneNested(item)
		}
	}
}

// canonicalTimestamps rewrites the OKF timestamp keys as UTC instants, so quoted
// and YAML-native forms and equivalent offsets compare equal.
func canonicalTimestamps(header map[string]any) {
	canonicalInstants(header, "stale_after")
	canonicalWindow(header)
	if generated, ok := header["generated"].(map[string]any); ok {
		canonicalInstants(generated, "at")
	}
	verified, _ := header["verified"].([]any)
	for _, entry := range verified {
		if event, ok := entry.(map[string]any); ok {
			canonicalInstants(event, "at")
		}
	}
	entries, _ := header["sources"].([]any)
	for _, entry := range entries {
		if source, ok := entry.(map[string]any); ok {
			canonicalInstants(source, "last_modified")
			canonicalWindow(source)
		}
	}
}

func canonicalWindow(owner map[string]any) {
	if window, ok := owner["usage_window"].(map[string]any); ok {
		canonicalInstants(window, "from", "to")
	}
}

func canonicalInstants(m map[string]any, keys ...string) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case time.Time:
			m[key] = v.UTC().Format(time.RFC3339Nano)
		case string:
			if parsed, ok := parseTimestampString(v); ok {
				m[key] = parsed.UTC().Format(time.RFC3339Nano)
			}
		}
	}
}

func emptyOptionalValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	default:
		return false
	}
}
