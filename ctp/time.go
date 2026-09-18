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
//
// Returns ErrInvalidTimeFormat on parse failure.
func ParseTime(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidTimeFormat
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
