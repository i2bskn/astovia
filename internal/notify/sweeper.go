package notify

import (
	"context"
	"log"
	"time"

	"github.com/i2bskn/astovia/internal/store"
)

// Sweeper dismisses task candidates left for longer than the user's
// candidate_expire_days, and event items whose start has passed. Unlike the
// Engine it covers every enabled user, with or without a push subscription.
// Pending notifications of the dismissed items are left to the Dispatcher,
// which does not send them for items that are not open.
type Sweeper struct {
	Store *store.Store
	Now   func() time.Time
}

// RunOnce dismisses the items due now. A failing user is logged and
// skipped; the next run tries again.
func (s *Sweeper) RunOnce(ctx context.Context) {
	now := s.Now().Unix()
	users, err := s.Store.CandidateSweepUserIDs(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("candidate sweep: list users: %v", err)
		}
		return
	}
	for _, uid := range users {
		if ctx.Err() != nil {
			return
		}
		if err = s.user(ctx, uid, now); err != nil && ctx.Err() == nil {
			log.Printf("candidate sweep: user=%s: %v", uid, err)
		}
	}
}

// user dismisses the passed events first, so that a candidate event is
// dismissed as passed rather than as expired.
func (s *Sweeper) user(ctx context.Context, userID string, now int64) error {
	settings, err := s.Store.NotificationSettings(ctx, userID)
	if err != nil {
		return err
	}
	if _, err = s.Store.ExpireEvents(ctx, userID, now); err != nil {
		return err
	}
	if settings.CandidateExpireDays <= 0 {
		return nil
	}
	_, err = s.Store.ExpireCandidates(ctx, userID, now-settings.CandidateExpireDays*86400, now)
	return err
}
