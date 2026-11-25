package okf

import (
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Timestamp reads an RFC 3339 date-time with an offset, or a YYYY-MM-DD date
// as midnight UTC.
func Timestamp(n *yaml.Node) (time.Time, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!timestamp") {
		return time.Time{}, false
	}
	layout := time.RFC3339
	if len(n.Value) == len(time.DateOnly) {
		layout = time.DateOnly
	}
	t, err := time.Parse(layout, strings.ToUpper(n.Value)) // RFC 3339 allows lowercase t and z.
	return t, err == nil
}
