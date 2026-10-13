package monitoring

import (
	"strconv"
	"strings"
	"time"
)

// formatUptime uses fixed elapsed-time units: 24-hour days, 30-day months,
// and 12-month (360-day) years. Subsecond precision is discarded.
func formatUptime(elapsed time.Duration) string {
	const day = 24 * time.Hour
	const month = 30 * day
	const year = 12 * month

	var result strings.Builder
	for _, unit := range []struct {
		duration time.Duration
		suffix   string
	}{
		{year, "y"},
		{month, "mo"},
		{day, "d"},
		{time.Hour, "h"},
		{time.Minute, "m"},
		{time.Second, "s"},
	} {
		value := elapsed / unit.duration
		elapsed %= unit.duration
		if value > 0 || result.Len() > 0 || unit.duration == time.Second {
			result.WriteString(strconv.FormatInt(int64(value), 10))
			result.WriteString(unit.suffix)
		}
	}
	return result.String()
}
