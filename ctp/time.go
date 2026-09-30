package ctp

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// TimeParseError is a structured error for time parsing failures.
type TimeParseError struct {
	Input string
	Msg   string
}

func (e TimeParseError) Error() string {
	return "ctp: invalid time format " + e.Input + ": " + e.Msg
}

func (e TimeParseError) Unwrap() error { return ErrInvalidTimeFormat }

// ErrInvalidTimeFormat indicates the time string couldn't be parsed.
var ErrInvalidTimeFormat = errors.New("invalid time format")

// ParseTime parses a time string in hh:mm:ss.ms, mm:ss.ms, ss.ms, or bare
// seconds format into total milliseconds (int). A bare number (no colons)
// is treated as seconds. Examples:
//
//	"1:30"       -> 90000   (1 min 30 sec)
//	"1:30.5"     -> 90500
//	"0:01:30"    -> 90000
//	"0:01:30.5"  -> 90500
//	"90"         -> 90000   (bare number = seconds)
//	"90.5"       -> 90500
//	"1m5s"       -> 65000   (unit form: h, m/min, s/sec, ms; any order of
//	"1m 5.5s"    -> 65500    largest first, spaces allowed)
//	"500ms"      -> 500
//
// Returns ErrInvalidTimeFormat on parse failure.
func ParseTime(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidTimeFormat
	}

	if ms, ok, err := parseUnitTime(s); ok {
		return ms, err
	}

	// Bare number (no colons) -> treat as seconds
	if !strings.Contains(s, ":") {
		sec, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec*1000 >= float64(math.MaxInt) {
			return 0, TimeParseError{Input: s, Msg: "not a valid number of seconds"}
		}
		return int(sec * 1000), nil
	}

	// Has colons: parse as hh:mm:ss[.ms] or mm:ss[.ms] or ss[.ms]
	parts := strings.Split(s, ":")
	var totalSec float64
	var err error

	switch len(parts) {
	case 3: // hh:mm:ss[.ms]
		h, err1 := strconv.Atoi(parts[0])
		m, err2 := strconv.Atoi(parts[1])
		ssec, err3 := strconv.ParseFloat(parts[2], 64)
		err = errors.Join(err1, err2, err3)
		if err != nil {
			return 0, TimeParseError{Input: s, Msg: "hours/minutes/seconds must be integers"}
		}
		if h < 0 || m < 0 || ssec < 0 {
			return 0, ErrInvalidTimeFormat
		}
		totalSec = float64(h)*3600 + float64(m)*60 + ssec
	case 2: // mm:ss[.ms]
		m, err1 := strconv.Atoi(parts[0])
		ssec, err2 := strconv.ParseFloat(parts[1], 64)
		err = errors.Join(err1, err2)
		if err != nil {
			return 0, TimeParseError{Input: s, Msg: "minutes/seconds must be integers"}
		}
		if m < 0 || ssec < 0 {
			return 0, ErrInvalidTimeFormat
		}
		totalSec = float64(m)*60 + ssec
	case 1: // ss[.ms] with trailing colon? shouldn't happen due to Contains(":"), but handle
		ssec, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return 0, TimeParseError{Input: s, Msg: "seconds must be a number"}
		}
		totalSec = ssec
	default:
		return 0, TimeParseError{Input: s, Msg: "too many colon-separated parts"}
	}

	if math.IsNaN(totalSec) || math.IsInf(totalSec, 0) || totalSec < 0 || totalSec*1000 >= float64(math.MaxInt) {
		return 0, ErrInvalidTimeFormat
	}
	return int(totalSec * 1000), nil
}

// FormatTime formats milliseconds as hh:mm:ss.ms (always 3 parts, ms padded to 3 digits).
// 0 -> "00:00:00.000"
func FormatTime(ms int) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms %= 3_600_000
	m := ms / 60_000
	ms %= 60_000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, ms)
}

var timeUnitMs = map[string]float64{"h": 3_600_000, "hr": 3_600_000, "m": 60_000, "min": 60_000, "s": 1000, "sec": 1000, "ms": 1}

// parseUnitTime reads "1h2m3.5s", "1m 5s", "500ms". ok=false when s has no
// unit letters at all (a clock or bare-seconds form, parsed by the caller).
func parseUnitTime(s string) (ms int, ok bool, err error) {
	lower := strings.ToLower(strings.ReplaceAll(s, " ", ""))
	if strings.IndexFunc(lower, func(r rune) bool { return r >= 'a' && r <= 'z' }) < 0 {
		return 0, false, nil
	}
	bad := TimeParseError{Input: s, Msg: "use a form like 1:05, 1m5s or 65"}
	total, lastRank := 0.0, 0.0
	for lower != "" {
		i := strings.IndexFunc(lower, func(r rune) bool { return r >= 'a' && r <= 'z' })
		if i <= 0 {
			return 0, true, bad
		}
		num, err := strconv.ParseFloat(lower[:i], 64)
		if err != nil || num < 0 {
			return 0, true, bad
		}
		j := i
		for j < len(lower) && lower[j] >= 'a' && lower[j] <= 'z' {
			j++
		}
		unit, found := timeUnitMs[lower[i:j]]
		// Units must shrink left to right ("1m5s", not "5s1m" or "1m1m").
		if !found || (lastRank != 0 && unit >= lastRank) {
			return 0, true, bad
		}
		lastRank = unit
		total += num * unit
		lower = lower[j:]
	}
	if math.IsInf(total, 0) || total >= float64(math.MaxInt) {
		return 0, true, bad
	}
	return int(math.Round(total)), true, nil
}
