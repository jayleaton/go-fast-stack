package adminrepo

import (
	"context"
	"time"

	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/appsql"
	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/model"
)

// Stats answers the analytics placeholder with counts over the main app's
// database (read-only). It implements services.Analytics (apps/admin).
type Stats struct {
	queries *appsql.Queries
	now     func() time.Time
}

// NewStats returns the statistics over the app database pool.
func NewStats(appPool appsql.DBTX) *Stats {
	return &Stats{queries: appsql.New(appPool), now: time.Now}
}

// Overview returns the headline counts.
func (stats *Stats) Overview(ctx context.Context) (model.Overview, error) {
	row, err := stats.queries.UserOverview(ctx)
	if err != nil {
		return model.Overview{}, err
	}
	return model.Overview{
		TotalUsers: row.TotalUsers, VerifiedUsers: row.VerifiedUsers, BannedUsers: row.BannedUsers, NewUsers7d: row.NewUsers7d,
		ActiveSessions: row.ActiveSessions, GeneratedAt: stats.now().UTC(),
	}, nil
}
