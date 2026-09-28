package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed 5-field cron expression (minute hour dom month dow).
type Schedule struct {
	minute, hour, dom, month, dow uint64
	domStar, dowStar              bool
}

var aliases = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *",
	"@weekly": "0 0 * * 0", "@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dowNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse parses a cron expression; supports *, lists, ranges, steps, names and @aliases.
func Parse(expr string) (*Schedule, error) {
	expr = strings.TrimSpace(expr)
	if a, ok := aliases[strings.ToLower(expr)]; ok {
		expr = a
	}
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron: esperado 5 campos, recebido %d em %q", len(f), expr)
	}
	s := &Schedule{}
	var err error
	if s.minute, err = field(f[0], 0, 59, nil); err != nil {
		return nil, err
	}
	if s.hour, err = field(f[1], 0, 23, nil); err != nil {
		return nil, err
	}
	if s.dom, err = field(f[2], 1, 31, nil); err != nil {
		return nil, err
	}
	if s.month, err = field(f[3], 1, 12, monthNames); err != nil {
		return nil, err
	}
	if s.dow, err = field(f[4], 0, 7, dowNames); err != nil {
		return nil, err
	}
	if s.dow&(1<<7) != 0 { // 7 = Sunday
		s.dow = (s.dow | 1) &^ (1 << 7)
	}
	s.domStar = f[2] == "*" || f[2] == "?"
	s.dowStar = f[4] == "*" || f[4] == "?"
	return s, nil
}

func field(expr string, lo, hi int, names map[string]int) (uint64, error) {
	var bits uint64
	for _, part := range strings.Split(expr, ",") {
		step := 1
		if base, st, ok := strings.Cut(part, "/"); ok {
			n, err := strconv.Atoi(st)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("cron: passo inválido %q", part)
			}
			step, part = n, base
		}
		start, end := lo, hi
		switch {
		case part == "*" || part == "?":
		case strings.Contains(part, "-"):
			a, b, _ := strings.Cut(part, "-")
			var err error
			if start, err = value(a, names); err != nil {
				return 0, err
			}
			if end, err = value(b, names); err != nil {
				return 0, err
			}
		default:
			v, err := value(part, names)
			if err != nil {
				return 0, err
			}
			start = v
			if step == 1 {
				end = v
			}
		}
		if start < lo || end > hi || start > end {
			return 0, fmt.Errorf("cron: valor fora do intervalo em %q", expr)
		}
		for v := start; v <= end; v += step {
			bits |= 1 << uint(v)
		}
	}
	return bits, nil
}

func value(s string, names map[string]int) (int, error) {
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("cron: valor inválido %q", s)
	}
	return v, nil
}

func (s *Schedule) dayMatches(t time.Time) bool {
	domOK := s.dom&(1<<uint(t.Day())) != 0
	dowOK := s.dow&(1<<uint(t.Weekday())) != 0
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowOK
	case s.dowStar:
		return domOK
	default:
		return domOK || dowOK // POSIX semantics
	}
}

// Next returns the first activation strictly after t (in t's location).
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<uint(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !s.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if s.hour&(1<<uint(t.Hour())) == 0 {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if s.minute&(1<<uint(t.Minute())) == 0 {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
