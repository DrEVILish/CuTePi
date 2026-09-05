package routes

import "testing"

func TestFormatClock(t *testing.T) {
	cases := map[float64]string{
		0:    "00:00",
		5:    "00:05",
		65:   "01:05",
		599:  "09:59",
		3599: "59:59",
		3600: "1:00:00",
		3661: "1:01:01",
		-5:   "00:00",
	}
	for in, want := range cases {
		if got := formatClock(in); got != want {
			t.Errorf("formatClock(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[int]string{
		0:         "0B",
		1023:      "1023B",
		1024:      "1.0KiB",
		1536:      "1.5KiB",
		1048576:   "1.0MiB",
		104857600: "100.0MiB",
	}
	for in, want := range cases {
		if got := formatSize(in); got != want {
			t.Errorf("formatSize(%v) = %q, want %q", in, got, want)
		}
	}
}
