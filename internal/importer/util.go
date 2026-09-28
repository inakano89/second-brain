package importer

import (
	"crypto/sha1"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// hashRef derives a short, stable reference from the given parts.
func hashRef(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		io.WriteString(h, p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05-07:00",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
	"2006_01_02",
	"2006/01/02 15:04:05",
	"2006/01/02",
	"02/01/2006 15:04:05",
	"02/01/2006 15:04",
	"02/01/2006",
	"20060102T150405Z",
	"20060102T150405",
	"20060102",
	"January 2, 2006 3:04 PM",
	"January 2, 2006 15:04",
	"January 2, 2006",
	"Jan 2, 2006",
	"2 Jan 2006",
	"2 January 2006",
	time.RFC1123Z,
	time.RFC1123,
	time.RFC850,
	time.ANSIC,
}

// parseDate accepts ISO/RFC dates, dd/mm/yyyy (pt-BR), English long dates and
// unix timestamps in seconds, milliseconds or microseconds.
func parseDate(s string, loc *time.Location) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if isDigits(s) && len(s) >= 9 {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil || v <= 0 {
			return time.Time{}, false
		}
		switch {
		case len(s) >= 16:
			return time.UnixMicro(v), true
		case len(s) >= 13:
			return time.UnixMilli(v), true
		}
		return time.Unix(v, 0), true
	}
	for _, l := range dateLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var accents = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func stripAccents(s string) string {
	out, _, err := transform.String(accents, s)
	if err != nil {
		return s
	}
	return out
}

// normKey normalizes a column/field name: lowercase, no accents, "_"/"-" → space.
func normKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\ufeff")))
	s = stripAccents(s)
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// splitList splits tag-like lists separated by comma, semicolon or pipe.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '|' }) {
		p = strings.TrimSpace(strings.Trim(strings.TrimSpace(p), `"'[]`))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// truthy interprets checkbox-like values (pt/en).
func truthy(s string) bool {
	switch normKey(s) {
	case "1", "true", "yes", "y", "x", "sim", "s", "done", "completed", "complete", "concluido", "concluida", "feito", "feita", "checked":
		return true
	}
	return false
}
