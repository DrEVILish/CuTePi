package ctp

import (
	"errors"
	"testing"
)

func TestParseTimeErrorsWrapSentinel(t *testing.T) {
	for _, in := range []string{"abc", "-5", "1:2:3:4", "", "1:x"} {
		if _, err := ParseTime(in); !errors.Is(err, ErrInvalidTimeFormat) {
			t.Errorf("ParseTime(%q) err=%v, want errors.Is ErrInvalidTimeFormat", in, err)
		}
	}
}
