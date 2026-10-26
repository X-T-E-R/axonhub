package httpclient

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultRetryAfterSeconds is the default cooldown duration when Retry-After header is missing or invalid.
	DefaultRetryAfterSeconds = 60

	// MaxRetryAfterDuration is the maximum cooldown duration to prevent channels from being unavailable for too long.
	MaxRetryAfterDuration = 5 * time.Minute
)

func IsNotFoundErr(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusNotFound
}

func IsRateLimitErr(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.StatusCode == http.StatusTooManyRequests
}

// HasRetryAfterHeader returns true if the error is a 429 rate limit error with a Retry-After header.
func HasRetryAfterHeader(err error) bool {
	var httpErr *Error
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		return false
	}

	return httpErr.Headers != nil && httpErr.Headers.Get("Retry-After") != ""
}

// ParseRetryAfter parses the Retry-After header from a 429 error according to RFC 7231.
// Returns (duration, true) if the error is a 429 rate limit error, (0, false) otherwise.
// If Retry-After header is missing or invalid, returns DefaultRetryAfterSeconds.
// The duration is capped at MaxRetryAfterDuration.
func ParseRetryAfter(err error) (time.Duration, bool) {
	var httpErr *Error
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}

	value := httpErr.Headers.Get("Retry-After")
	if value == "" {
		return DefaultRetryAfterSeconds * time.Second, true
	}

	// 1. Try to parse as integer seconds (delta-seconds)
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds <= 0 {
			return 0, true
		}

		duration := time.Duration(seconds) * time.Second

		return min(duration, MaxRetryAfterDuration), true
	}

	// 2. Try to parse as HTTP-date
	if t, err := http.ParseTime(value); err == nil {
		duration := time.Until(t)
		if duration <= 0 {
			return 0, true
		}

		return min(duration, MaxRetryAfterDuration), true
	}

	// 3. Failed to parse, use default
	return DefaultRetryAfterSeconds * time.Second, true
}

// RetryAfterHeader returns a valid end-to-end Retry-After value for public
// responses. Unlike ParseRetryAfter, it preserves the upstream advice without
// applying a cooldown default, duration conversion, or cap.
func RetryAfterHeader(headers http.Header) string {
	values := headers.Values("Retry-After")
	if len(values) != 1 {
		return ""
	}
	for _, connection := range headers.Values("Connection") {
		for token := range strings.SplitSeq(connection, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "Retry-After") {
				return ""
			}
		}
	}
	value := strings.Trim(values[0], " \t")
	if value == "" {
		return ""
	}
	// RFC 9110 delay-seconds is 1*DIGIT, not a signed or fractional number.
	// Check the syntax without integer conversion so large valid values survive.
	digits := true
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			digits = false
			break
		}
	}
	if digits {
		return value
	}
	if _, err := http.ParseTime(value); err == nil {
		return value
	}
	return ""
}

type Error struct {
	Method     string      `json:"method"`
	URL        string      `json:"url"`
	StatusCode int         `json:"status_code"`
	Status     string      `json:"status"`
	Body       []byte      `json:"body"`
	Headers    http.Header `json:"-"` // HTTP response headers (not serialized)
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s - %s with status %s", e.Method, e.URL, e.Status)
}
