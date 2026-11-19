package okf

import (
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Timestamp reads an OKF date-time with an offset or a date at midnight UTC.
func Timestamp(n *yaml.Node) (time.Time, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!timestamp") {
		return time.Time{}, false
	}
	return parseTimestampString(n.Value)
}

// time.Parse also accepts single-digit hours, comma fractions, and out-of-range
// zone offsets. Match the RFC 3339 spelling before checking the calendar value.
var dateTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func parseTimestampString(value string) (time.Time, bool) {
	layout := time.DateOnly
	if len(value) != len(time.DateOnly) {
		if !dateTime.MatchString(value) {
			return time.Time{}, false
		}
		layout = time.RFC3339
		value = strings.ToUpper(value)
	}
	parsed, err := time.Parse(layout, value)
	return parsed, err == nil
}
