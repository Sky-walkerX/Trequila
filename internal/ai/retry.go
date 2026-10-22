package ai

import (
	"context"
	"strings"
	"time"
)

// IsAuthError reports whether an error indicates a permanent, user-fixable
// problem with their key: bad/expired key, no credits, or quota exhaustion.
// Callers MUST check this BEFORE IsRetryable (some messages match both).
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{
		"401", "403", "invalid api key", "api key not valid", "incorrect api key",
		"unauthenticated", "permission denied", "insufficient", "insufficient_quota",
		"billing", "exceeded your current quota", "account is not active",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// IsRetryable reports whether an error is transient and worth retrying.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, s := range []string{
		"429", "500", "502", "503", "504", "resource exhausted",
		"unavailable", "overloaded", "timeout", "deadline exceeded", "connection reset",
	} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// Retry runs fn with exponential backoff (1s, 2s, 4s, ...). It stops early on
// success, on a permanent auth error, or on a non-retryable error.
func Retry[T any](ctx context.Context, maxRetries int, fn func(context.Context) (T, error)) (T, error) {
	var result T
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result, err = fn(ctx)
		if err == nil {
			return result, nil
		}
		if IsAuthError(err) || !IsRetryable(err) || attempt == maxRetries {
			return result, err
		}
		backoff := time.Duration(1<<uint(attempt)) * time.Second
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	return result, err
}
