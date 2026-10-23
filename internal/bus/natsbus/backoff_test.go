package natsbus

import (
	"testing"
	"time"
)

func TestRetryDelayDoublesAndIsCapped(t *testing.T) {
	base := 2 * time.Second
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute}
	for i, w := range want {
		if got := retryDelay(base, uint64(i+1)); got != w {
			t.Errorf("delivery %d: delay = %v, want %v", i+1, got, w)
		}
	}
	if got := retryDelay(base, 0); got != base {
		t.Errorf("unknown delivery count: delay = %v, want %v", got, base)
	}
	if got := retryDelay(base, 1<<40); got != time.Minute {
		t.Errorf("huge delivery count: delay = %v, want the cap", got)
	}
}
