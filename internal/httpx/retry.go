package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func RetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds <= 0 {
			return 0
		}
		if seconds > int64((1<<63-1)/int64(time.Second)) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}
