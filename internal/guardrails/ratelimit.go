package guardrails

import (
	"context"
	"fmt"
	"time"
)

// CounterStore increments and returns the counter for key within the window
// starting at windowStart; the item expires after ttl. Implementations:
// DynamoDB in production, an in-memory fake in tests.
type CounterStore interface {
	IncrementWindow(ctx context.Context, key string, windowStart time.Time, ttl time.Duration) (int, error)
}

// Limiter enforces sliding per-hour and per-day windows (PRD §8).
type Limiter struct {
	Store   CounterStore
	PerHour int
	PerDay  int
	Now     func() time.Time
}

// Check increments both windows for tool and decides. A store failure blocks
// the send (fail closed - the limiter is the cost control).
func (l *Limiter) Check(ctx context.Context, tool string) Decision {
	name := "rate_limit"
	now := time.Now()
	if l.Now != nil {
		now = l.Now()
	}
	hourStart := now.Truncate(time.Hour)
	dayStart := now.Truncate(24 * time.Hour)
	hourCount, err := l.Store.IncrementWindow(ctx, tool+"#hour#"+hourStart.Format(time.RFC3339), hourStart, 2*time.Hour)
	if err != nil {
		return Decision{Name: name, Allowed: false, Reason: "rate-limit store unavailable: " + err.Error()}
	}
	dayCount, err := l.Store.IncrementWindow(ctx, tool+"#day#"+dayStart.Format(time.RFC3339), dayStart, 48*time.Hour)
	if err != nil {
		return Decision{Name: name, Allowed: false, Reason: "rate-limit store unavailable: " + err.Error()}
	}
	if l.PerHour > 0 && hourCount > l.PerHour {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("hourly limit reached (%d/%d)", hourCount, l.PerHour)}
	}
	if l.PerDay > 0 && dayCount > l.PerDay {
		return Decision{Name: name, Allowed: false,
			Reason: fmt.Sprintf("daily limit reached (%d/%d)", dayCount, l.PerDay)}
	}
	return Decision{Name: name, Allowed: true,
		Reason: fmt.Sprintf("%d/%d this hour, %d/%d today", hourCount, l.PerHour, dayCount, l.PerDay)}
}
