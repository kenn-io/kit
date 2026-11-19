package okf

import "time"

// NormalizedMetadata returns a fresh mapping using the value rules in OKF v0.3
// §11.2. Known list fields become slices and known timestamps become UTC strings.
// It preserves the contents of extension values.
func (d *Document) NormalizedMetadata() (map[string]any, error) {
	fields := make(map[string]any)
	if err := d.Metadata.Decode(&fields); err != nil {
		return nil, err
	}
	normalizeFields(fields, "")
	return fields, nil
}

func normalizeFields(fields map[string]any, family string) {
	for key, value := range fields {
		if absent(value) {
			delete(fields, key)
			continue
		}
		switch family + "." + key {
		case ".tags", ".sources", ".verified", ".parameters":
			items, ok := value.([]any)
			if !ok {
				items = []any{value}
				fields[key] = items
			}
			for _, item := range items {
				if entry, ok := item.(map[string]any); ok {
					normalizeFields(entry, key)
				}
			}
		case ".generated", ".usage_window", "sources.usage_window", ".executor", ".attester":
			if entry, ok := value.(map[string]any); ok {
				normalizeFields(entry, key)
				if len(entry) == 0 {
					delete(fields, key)
				}
			}
		case ".stale_after", "generated.at", "verified.at", "sources.last_modified", "usage_window.from", "usage_window.to":
			switch stamp := value.(type) {
			case time.Time:
				fields[key] = stamp.UTC().Format(time.RFC3339Nano)
			case string:
				if t, ok := parseTimestampString(stamp); ok {
					fields[key] = t.UTC().Format(time.RFC3339Nano)
				}
			}
		}
	}
}

func absent(value any) bool {
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
