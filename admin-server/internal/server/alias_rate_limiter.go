package server

import (
	"sync"
	"time"
)

// aliasCreateRateLimiter applies the administrator-selected minimum interval
// per account and preserves any temporary block returned by Apple.
type aliasCreateRateLimiter struct {
	mu           sync.Mutex
	lastSuccess  map[string]time.Time
	blockedUntil map[string]time.Time
}

func newAliasCreateRateLimiter() *aliasCreateRateLimiter {
	return &aliasCreateRateLimiter{
		lastSuccess:  make(map[string]time.Time),
		blockedUntil: make(map[string]time.Time),
	}
}

func (l *aliasCreateRateLimiter) retryAfter(accountID string, now time.Time, minimumInterval time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if blockedUntil := l.blockedUntil[accountID]; blockedUntil.After(now) {
		return blockedUntil.Sub(now)
	}
	delete(l.blockedUntil, accountID)

	if minimumInterval <= 0 {
		return 0
	}
	readyAt := l.lastSuccess[accountID].Add(minimumInterval)
	if readyAt.After(now) {
		return readyAt.Sub(now)
	}
	return 0
}

func (l *aliasCreateRateLimiter) recordSuccess(accountID string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastSuccess[accountID] = now
}

func (l *aliasCreateRateLimiter) block(accountID string, now time.Time, retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	until := now.Add(retryAfter)
	if until.After(l.blockedUntil[accountID]) {
		l.blockedUntil[accountID] = until
	}
}
