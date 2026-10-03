package main

import (
	"strconv"
	"time"
)

// tsLayout is RFC 3339 in UTC with fixed-width milliseconds, so stored times sort as text.
const tsLayout = "2006-01-02T15:04:05.000Z"

func ts(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(tsLayout, s)
	return t, err == nil
}

// fmtTime shows a stored time in UTC with layout, or "" when unset.
func fmtTime(s, layout string) string {
	t, ok := parseTS(s)
	if !ok {
		return s
	}
	return t.Format(layout)
}

func olderThan(now time.Time, s string, d time.Duration) bool {
	t, ok := parseTS(s)
	return ok && now.Sub(t) > d
}

// age is a short relative time: "40s", "12m", "3h", "2d".
func age(now time.Time, s string) string {
	t, ok := parseTS(s)
	if !ok {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
