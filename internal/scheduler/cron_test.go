package scheduler

import (
	"testing"
	"time"
)

func TestCronNext(t *testing.T) {
	loc, _ := time.LoadLocation("America/Sao_Paulo")
	base := time.Date(2026, 9, 28, 6, 59, 30, 0, loc) // Monday
	cases := []struct {
		spec string
		want time.Time
	}{
		{"0 7 * * *", time.Date(2026, 9, 28, 7, 0, 0, 0, loc)},
		{"*/15 * * * *", time.Date(2026, 9, 28, 7, 0, 0, 0, loc)},
		{"0 18 * * 0", time.Date(2026, 10, 4, 18, 0, 0, 0, loc)},
		{"0 18 * * 7", time.Date(2026, 10, 4, 18, 0, 0, 0, loc)},
		{"30 3 1 * *", time.Date(2026, 10, 1, 3, 30, 0, 0, loc)},
		{"0 9 * * mon-fri", time.Date(2026, 9, 28, 9, 0, 0, 0, loc)},
		{"@hourly", time.Date(2026, 9, 28, 7, 0, 0, 0, loc)},
		{"0 */4 * * *", time.Date(2026, 9, 28, 8, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		s, err := Parse(c.spec)
		if err != nil {
			t.Fatalf("%s: %v", c.spec, err)
		}
		if got := s.Next(base); !got.Equal(c.want) {
			t.Errorf("%s: got %v want %v", c.spec, got, c.want)
		}
	}
	for _, bad := range []string{"* * *", "60 * * * *", "* * * * 8", "*/0 * * * *"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
