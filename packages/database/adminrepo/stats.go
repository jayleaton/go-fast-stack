package adminrepo

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/appsql"
	"github.com/jayleaton/ai-patterns-go/packages/database/adminrepo/model"
)

// Stats answers the analytics placeholder with counts over the main app's
// database (read-only). It implements services.Analytics (apps/admin).
type Stats struct {
	q   *appsql.Queries
	now func() time.Time
}

// NewStats returns the statistics over the app database pool.
func NewStats(appPool *pgxpool.Pool) *Stats {
	return &Stats{q: appsql.New(appPool), now: time.Now}
}

// Overview returns the headline counts.
func (s *Stats) Overview(ctx context.Context) (model.Overview, error) {
	r, err := s.q.UserOverview(ctx)
	if err != nil {
		return model.Overview{}, err
	}
	return model.Overview{
		TotalUsers: r.TotalUsers, VerifiedUsers: r.VerifiedUsers, BannedUsers: r.BannedUsers, NewUsers7d: r.NewUsers7d,
		ActiveSessions: r.ActiveSessions, GeneratedAt: s.now().UTC(),
	}, nil
}
