package server

import (
	"testing"
	"time"
)

func TestAliasCreateRateLimiterUsesCustomInterval(t *testing.T) {
	limiter := newAliasCreateRateLimiter()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	if got := limiter.retryAfter("account-a", now, 30*time.Second); got != 0 {
		t.Fatalf("first attempt unexpectedly limited for %s", got)
	}
	limiter.recordSuccess("account-a", now)
	if got := limiter.retryAfter("account-a", now.Add(10*time.Second), 30*time.Second); got != 20*time.Second {
		t.Fatalf("retry after = %s, want 20s", got)
	}
	if got := limiter.retryAfter("account-b", now, 30*time.Second); got != 0 {
		t.Fatalf("another account was limited for %s", got)
	}
	if got := limiter.retryAfter("account-a", now.Add(10*time.Second), 5*time.Second); got != 0 {
		t.Fatalf("shortened custom interval still limited for %s", got)
	}
	if got := limiter.retryAfter("account-a", now, 0); got != 0 {
		t.Fatalf("zero custom interval unexpectedly limited for %s", got)
	}
}

func TestAliasCreateRateLimiterHonorsAppleBlock(t *testing.T) {
	limiter := newAliasCreateRateLimiter()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	limiter.block("account-a", now, 25*time.Minute)
	if got := limiter.retryAfter("account-a", now.Add(5*time.Minute), 0); got != 20*time.Minute {
		t.Fatalf("retry after = %s, want 20m", got)
	}
	if got := limiter.retryAfter("account-a", now.Add(25*time.Minute), 0); got != 0 {
		t.Fatalf("expired Apple block still limited for %s", got)
	}
}
