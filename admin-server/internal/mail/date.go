package mail

import (
	"strconv"
	"strings"
	"time"
)

var chinaStandardTime = time.FixedZone("CST", 8*60*60)

// ParseMessageDate accepts the timestamp shapes returned by webmail APIs and
// standard mail Date headers. Values without an explicit zone are interpreted
// as China Standard Time because 163 webmail emits local wall-clock values.
func ParseMessageDate(value string) (time.Time, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return time.Time{}, false
	}

	if number, err := strconv.ParseInt(raw, 10, 64); err == nil {
		var parsed time.Time
		switch magnitude := absInt64(number); {
		case magnitude >= 1_000_000_000_000_000_000:
			parsed = time.Unix(0, number)
		case magnitude >= 1_000_000_000_000_000:
			parsed = time.UnixMicro(number)
		case magnitude >= 1_000_000_000_000:
			parsed = time.UnixMilli(number)
		default:
			parsed = time.Unix(number, 0)
		}
		if validMessageYear(parsed) {
			return parsed, true
		}
	}

	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02 15:04:05 MST",
		"Mon, 2 Jan 2006 15:04:05 MST",
	} {
		if parsed, err := time.ParseInLocation(layout, raw, chinaStandardTime); err == nil && validMessageYear(parsed) {
			return parsed, true
		}
	}

	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		time.RFC822,
		time.RFC850,
		time.ANSIC,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"2 Jan 2006 15:04:05 -0700",
		"2006-01-02 15:04:05 -0700",
	} {
		if parsed, err := time.Parse(layout, raw); err == nil && validMessageYear(parsed) {
			return parsed, true
		}
	}

	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006/01/02 15:04:05",
		"2006-1-2 15:04:05",
		"2006-01-02 15:04",
		"2006/01/02 15:04",
	} {
		if parsed, err := time.ParseInLocation(layout, raw, chinaStandardTime); err == nil && validMessageYear(parsed) {
			return parsed, true
		}
	}

	return time.Time{}, false
}

func NormalizeMessageDate(value string) string {
	parsed, ok := ParseMessageDate(value)
	if !ok {
		return strings.TrimSpace(value)
	}
	return parsed.UTC().Format(time.RFC3339Nano)
}

func validMessageYear(value time.Time) bool {
	year := value.UTC().Year()
	return year >= 1970 && year <= 3000
}

func absInt64(value int64) uint64 {
	if value < 0 {
		return uint64(-(value + 1)) + 1
	}
	return uint64(value)
}
