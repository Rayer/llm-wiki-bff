package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	rateLimitRetryAfter = 60
	cleanupInterval     = 5 * time.Minute
)

type rateLimitEntry struct {
	mu       sync.Mutex
	requests []time.Time
}

func NewRateLimiter(limit int, window time.Duration) gin.HandlerFunc {
	ips := sync.Map{}

	go func() {
		for range time.NewTicker(cleanupInterval).C {
			ips.Range(func(key, value any) bool {
				entry := value.(*rateLimitEntry)
				entry.mu.Lock()
				cutoff := time.Now().Add(-window)
				keep := entry.requests[:0]
				for _, t := range entry.requests {
					if t.After(cutoff) {
						keep = append(keep, t)
					}
				}
				entry.requests = keep
				empty := len(keep) == 0
				entry.mu.Unlock()
				if empty {
					ips.Delete(key)
				}
				return true
			})
		}
	}()

	return func(c *gin.Context) {
		ip := c.GetHeader("CF-Connecting-IP")
		if ip == "" {
			ip = c.GetHeader("X-Forwarded-For")
		}
		if ip == "" {
			ip = c.ClientIP()
		}

		val, _ := ips.LoadOrStore(ip, &rateLimitEntry{})
		entry := val.(*rateLimitEntry)

		now := time.Now()
		entry.mu.Lock()
		cutoff := now.Add(-window)
		keep := entry.requests[:0]
		for _, t := range entry.requests {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		entry.requests = keep

		if len(entry.requests) >= limit {
			entry.mu.Unlock()
			c.Header("Retry-After", strconv.Itoa(rateLimitRetryAfter))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "rate_limit_exceeded",
				"retry_after": rateLimitRetryAfter,
			})
			return
		}
		entry.requests = append(entry.requests, now)
		entry.mu.Unlock()
		c.Next()
	}
}
