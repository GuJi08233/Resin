package warp

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRateLimitBackoff = 30 * time.Second
	maxRateLimitBackoff     = 5 * time.Minute
	maxServerRetryAfter     = time.Hour
)

// RateLimitError means Cloudflare temporarily stopped registrations, rather
// than the selected parent failing to carry a WARP tunnel. Schedulers should
// defer creation globally until RetryAt without penalizing that parent.
type RateLimitError struct {
	RetryAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("Cloudflare API HTTP 429; registration paused until %s", e.RetryAt.UTC().Format(time.RFC3339))
}

// RetryAfter recognizes rate limits through wrapped factory errors. Expired
// limits still return true with zero delay, so callers can distinguish them
// from transport failures without relying on the error message.
func RetryAfter(err error) (time.Duration, bool) {
	var limited *RateLimitError
	if !errors.As(err, &limited) || limited == nil {
		return 0, false
	}
	return max(0, time.Until(limited.RetryAt)), true
}

func serverRetryDelay(header string, now time.Time) (time.Duration, bool) {
	header = strings.TrimSpace(header)
	if seconds, err := strconv.ParseUint(header, 10, 64); err == nil {
		if seconds > uint64(maxServerRetryAfter/time.Second) {
			return maxServerRetryAfter, true
		}
		return max(time.Second, time.Duration(seconds)*time.Second), true
	} else if errors.Is(err, strconv.ErrRange) && strings.Trim(header, "0123456789") == "" {
		return maxServerRetryAfter, true
	}
	if until, err := http.ParseTime(header); err == nil {
		return min(maxServerRetryAfter, max(time.Second, until.Sub(now))), true
	}
	return 0, false
}

func (r *registrar) rateLimited(header string) *RateLimitError {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rateFailures++
	delay, supplied := serverRetryDelay(header, now)
	if !supplied {
		delay = defaultRateLimitBackoff
		for attempt := 1; attempt < r.rateFailures && delay < maxRateLimitBackoff; attempt++ {
			delay = min(maxRateLimitBackoff, delay*2)
		}
	}
	until := now.Add(delay)
	// Concurrent responses may arrive out of order. Neither a shorter server
	// hint nor a previously admitted registration can shorten an existing pause.
	if r.cooldownUntil.After(until) {
		until = r.cooldownUntil
	}
	if r.next.After(until) {
		until = r.next
	}
	r.cooldownUntil, r.next = until, until
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
	return &RateLimitError{RetryAt: until}
}

func (r *registrar) currentRateLimitLocked(now time.Time) *RateLimitError {
	if r.cooldownUntil.After(now) {
		return &RateLimitError{RetryAt: r.cooldownUntil}
	}
	return nil
}
