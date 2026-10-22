// Package hours evaluates OpenStreetMap opening_hours values.
//
// OSM's opening_hours grammar is large (sunrise and sunset offsets, week numbers,
// months, holidays, comments). This package supports the subset that covers the vast
// majority of real-world tags: days, day ranges and lists, one or more time ranges per
// day, overnight ranges, "off", "24/7", and later rules overriding earlier ones. Public
// and school holiday clauses are accepted and ignored.
//
// Anything outside that subset is reported as ErrUnsupported, and Evaluate turns it into
// Unknown. A caller must treat Unknown as "can't tell", never as "closed": wrongly
// excluding an open place is a worse failure than showing one whose hours are unknown.
package hours

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported means the value uses syntax this package does not interpret.
var ErrUnsupported = errors.New("hours: unsupported opening_hours syntax")

// State is whether a place is open at a moment.
type State int

// Possible states. Unknown is the zero value on purpose: no information is not "closed".
const (
	Unknown State = iota
	Open
	Closed
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case Closed:
		return "closed"
	default:
		return "unknown"
	}
}

// Window is one continuous open interval. It may end on the day after it starts.
type Window struct{ Start, End time.Time }

// span is minutes since midnight of the day it starts on; end may exceed 1440 for an
// overnight range.
type span struct{ start, end int }

// Schedule is a parsed opening_hours value, indexed by time.Weekday.
type Schedule struct {
	days [7][]span
}

var (
	dayNames  = map[string]time.Weekday{"mo": time.Monday, "tu": time.Tuesday, "we": time.Wednesday, "th": time.Thursday, "fr": time.Friday, "sa": time.Saturday, "su": time.Sunday}
	timeRange = regexp.MustCompile(`^(\d{1,2}):(\d{2})-(\d{1,2}):(\d{2})$`)
	dayToken  = regexp.MustCompile(`^(?i)(mo|tu|we|th|fr|sa|su|ph|sh)\b`)
)

// Parse reads an opening_hours value.
func Parse(raw string) (Schedule, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Schedule{}, fmt.Errorf("%w: empty value", ErrUnsupported)
	}
	if raw == "24/7" {
		var s Schedule
		for d := range s.days {
			s.days[d] = []span{{0, 1440}}
		}
		return s, nil
	}

	var s Schedule
	covered := false
	for _, chunk := range strings.Split(raw, ";") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		for _, rule := range splitRules(chunk) {
			applied, err := s.apply(rule)
			if err != nil {
				return Schedule{}, err
			}
			covered = covered || applied
		}
	}
	if !covered {
		return Schedule{}, fmt.Errorf("%w: no weekday is described", ErrUnsupported)
	}
	return s, nil
}

// splitRules splits "Mo-Fr 09:00-17:00, Sa 10:00-14:00" into its two rules while leaving
// "Mo-Fr 09:00-12:00, 14:00-18:00" (more ranges) and "Mo,We,Fr 10:00-16:00" (a day list)
// whole. A comma separates rules only when it follows a time range or "off" and the next
// word is a day.
func splitRules(chunk string) []string {
	var out []string
	prev := 0
	for i := 0; i < len(chunk); i++ {
		if chunk[i] != ',' {
			continue
		}
		before := strings.ToLower(strings.TrimSpace(chunk[prev:i]))
		after := strings.TrimSpace(chunk[i+1:])
		endsRule := before != "" && (before[len(before)-1] >= '0' && before[len(before)-1] <= '9' ||
			strings.HasSuffix(before, "off") || strings.HasSuffix(before, "closed"))
		if endsRule && dayToken.MatchString(after) {
			out = append(out, strings.TrimSpace(chunk[prev:i]))
			prev = i + 1
		}
	}
	return append(out, strings.TrimSpace(chunk[prev:]))
}

// apply interprets one rule and reports whether it described at least one weekday.
func (s *Schedule) apply(rule string) (bool, error) {
	fields := strings.Fields(rule)
	if len(fields) == 0 {
		return false, nil
	}

	var days []time.Weekday
	hasDaySelector := false
	holidayOnly := false
	rest := fields
	if startsWithLetter(fields[0]) && !isStateWord(fields[0]) {
		d, holidays, err := parseDays(fields[0])
		if err != nil {
			return false, err
		}
		days, hasDaySelector, holidayOnly = d, true, holidays && len(d) == 0
		rest = fields[1:]
	}
	if !hasDaySelector {
		days = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday}
	}
	if len(rest) == 0 {
		return false, fmt.Errorf("%w: rule %q has no times", ErrUnsupported, rule)
	}

	body := strings.Join(rest, " ")
	if lower := strings.ToLower(body); lower == "off" || lower == "closed" {
		if holidayOnly {
			return false, nil // "PH off": nothing to say about ordinary weekdays
		}
		for _, d := range days {
			s.days[d] = nil
		}
		return true, nil
	}

	spans, err := parseSpans(body)
	if err != nil {
		return false, err
	}
	if holidayOnly {
		return false, nil
	}
	for _, d := range days {
		s.days[d] = spans
	}
	return true, nil
}

func startsWithLetter(s string) bool {
	c := s[0]
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

func isStateWord(s string) bool {
	switch strings.ToLower(s) {
	case "off", "closed", "open":
		return true
	}
	return false
}

// parseDays reads "Mo-Fr", "Sa,Su", "Mo,We-Fr", "Fr-Mo" and "Mo-Fr,PH". The boolean
// reports whether a holiday token (PH, SH) was present; those tokens select no weekday.
func parseDays(sel string) ([]time.Weekday, bool, error) {
	var out []time.Weekday
	holidays := false
	for _, item := range strings.Split(sel, ",") {
		item = strings.ToLower(strings.TrimSpace(item))
		switch item {
		case "":
			return nil, false, fmt.Errorf("%w: empty day in %q", ErrUnsupported, sel)
		case "ph", "sh":
			holidays = true
			continue
		}
		if a, b, isRange := strings.Cut(item, "-"); isRange {
			from, ok1 := dayNames[a]
			to, ok2 := dayNames[b]
			if !ok1 || !ok2 {
				return nil, false, fmt.Errorf("%w: day range %q", ErrUnsupported, item)
			}
			for d := from; ; d = (d + 1) % 7 {
				out = append(out, d)
				if d == to {
					break
				}
			}
			continue
		}
		d, ok := dayNames[item]
		if !ok {
			return nil, false, fmt.Errorf("%w: day %q", ErrUnsupported, item)
		}
		out = append(out, d)
	}
	return out, holidays, nil
}

// parseSpans reads "09:00-12:00,14:00-18:00" (commas and spaces both separate ranges).
func parseSpans(body string) ([]span, error) {
	var out []span
	for _, tok := range strings.FieldsFunc(body, func(r rune) bool { return r == ',' || r == ' ' }) {
		m := timeRange.FindStringSubmatch(tok)
		if m == nil {
			return nil, fmt.Errorf("%w: time %q", ErrUnsupported, tok)
		}
		sh, _ := strconv.Atoi(m[1])
		sm, _ := strconv.Atoi(m[2])
		eh, _ := strconv.Atoi(m[3])
		em, _ := strconv.Atoi(m[4])
		if sh > 24 || eh > 24 || sm > 59 || em > 59 || (sh == 24 && sm != 0) || (eh == 24 && em != 0) {
			return nil, fmt.Errorf("%w: time out of range %q", ErrUnsupported, tok)
		}
		start, end := sh*60+sm, eh*60+em
		if end <= start {
			end += 1440 // runs past midnight
		}
		out = append(out, span{start, end})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no times in %q", ErrUnsupported, body)
	}
	return out, nil
}

// StateAt reports whether the place is open at t, judged on t's own wall clock (give it
// the place's local time).
func (s Schedule) StateAt(t time.Time) State {
	minute := t.Hour()*60 + t.Minute()
	for _, sp := range s.days[t.Weekday()] {
		if minute >= sp.start && minute < min(sp.end, 1440) {
			return Open
		}
	}
	prev := (t.Weekday() + 6) % 7
	for _, sp := range s.days[prev] {
		if sp.end > 1440 && minute < sp.end-1440 {
			return Open
		}
	}
	return Closed
}

// Windows lists the open intervals that start on the local date of d. An overnight
// interval ends on the following day.
func (s Schedule) Windows(d time.Time) []Window {
	y, m, day := d.Date()
	loc := d.Location()
	var out []Window
	for _, sp := range s.days[d.Weekday()] {
		out = append(out, Window{
			Start: time.Date(y, m, day, 0, sp.start, 0, 0, loc),
			End:   time.Date(y, m, day, 0, sp.end, 0, 0, loc),
		})
	}
	return out
}

// OpenOn reports whether the place is open at any point on the local date of d,
// including the early hours of that date when yesterday's hours run past midnight.
func (s Schedule) OpenOn(d time.Time) bool {
	if len(s.days[d.Weekday()]) > 0 {
		return true
	}
	prev := (d.Weekday() + 6) % 7
	for _, sp := range s.days[prev] {
		if sp.end > 1440 {
			return true
		}
	}
	return false
}

// Evaluate is Parse followed by StateAt, returning Unknown when raw is empty or uses
// syntax that is not supported.
func Evaluate(raw string, t time.Time) State {
	s, err := Parse(raw)
	if err != nil {
		return Unknown
	}
	return s.StateAt(t)
}
