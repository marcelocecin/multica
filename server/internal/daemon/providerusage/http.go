package providerusage

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	vendorTimeout     = 15 * time.Second
	minRateLimitWait  = 60 * time.Second
	maxRateLimitWait  = 30 * time.Minute
	maxVendorBodySize = 1 << 20
)

func readVendorBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxVendorBodySize))
}

// classifyVendorStatus maps a vendor HTTP status onto an upload decision.
// 401 and 403 are an empty snapshot. 429 backs off and keeps the last good
// snapshot. Other non-200 statuses are transient.
func classifyVendorStatus(status int, retryAfter string) (reason string, backoff time.Duration, transient bool) {
	switch status {
	case http.StatusOK:
		return "", 0, false
	case http.StatusUnauthorized, http.StatusForbidden:
		return ReasonUnauthorized, 0, false
	case http.StatusTooManyRequests:
		return "", parseRetryAfter(retryAfter), true
	default:
		return "", 0, true
	}
}

func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return minRateLimitWait
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		return clampBackoff(time.Duration(secs) * time.Second)
	}
	if when, err := http.ParseTime(header); err == nil {
		d := time.Until(when)
		if d < 0 {
			d = minRateLimitWait
		}
		return clampBackoff(d)
	}
	return minRateLimitWait
}

func clampBackoff(d time.Duration) time.Duration {
	if d < minRateLimitWait {
		return minRateLimitWait
	}
	if d > maxRateLimitWait {
		return maxRateLimitWait
	}
	return d
}
