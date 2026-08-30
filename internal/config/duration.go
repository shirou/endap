package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseDuration parses durations of the form "30d", "2y", "12h", "90m".
//
// time.ParseDuration is not enough on its own: its unit set stops at h, so the
// day and year units the flags are written in ("--since 30d") are rejected
// outright. Anything it does understand is still handed to it.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	const (
		day  = 24 * time.Hour
		week = 7 * day
		year = 365 * day
	)
	units := []struct {
		suffix string
		unit   time.Duration
	}{
		{"y", year},
		{"w", week},
		{"d", day},
	}
	for _, u := range units {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
		if err != nil {
			return 0, fmt.Errorf("bad duration %q: %v", s, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("bad duration %q: must not be negative", s)
		}
		return time.Duration(n * float64(u.unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q (use ns/us/ms/s/m/h/d/w/y)", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("bad duration %q: must not be negative", s)
	}
	return d, nil
}
