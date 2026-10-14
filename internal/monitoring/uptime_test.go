package monitoring

import (
	"testing"
	"time"
)

func TestFormatUptime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    string
	}{
		{"zero", 0, "0s"},
		{"subsecond", 999 * time.Millisecond, "0s"},
		{"seconds", 59999 * time.Millisecond, "59s"},
		{"minute", time.Minute, "1m0s"},
		{"hour", time.Hour, "1h0m0s"},
		{"before day", 24*time.Hour - time.Nanosecond, "23h59m59s"},
		{"day", 24 * time.Hour, "1d0h0m0s"},
		{"example", 29*time.Hour + 40*time.Minute + 687159078*time.Nanosecond, "1d5h40m0s"},
		{"before month", 720*time.Hour - time.Nanosecond, "29d23h59m59s"},
		{"month", 720 * time.Hour, "1mo0d0h0m0s"},
		{"before year", 8640*time.Hour - time.Nanosecond, "11mo29d23h59m59s"},
		{"year", 8640 * time.Hour, "1y0mo0d0h0m0s"},
		{"mixed", 18869*time.Hour + 6*time.Minute + 7*time.Second, "2y2mo6d5h6m7s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatUptime(tc.elapsed); got != tc.want {
				t.Fatalf("formatUptime(%v) = %q, want %q", tc.elapsed, got, tc.want)
			}
		})
	}
}
