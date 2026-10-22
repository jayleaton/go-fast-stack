package api_test

import (
	"testing"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api/apitest"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
)

var overview = services.Overview{
	TotalUsers: 10, VerifiedUsers: 8, BannedUsers: 1, NewUsers7d: 3, ActiveSessions: 12,
	GeneratedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
}

var analyticsRoutes = map[string]route{
	"GET analytics": {
		method: "GET", path: r.Analytics,
		expect: func(m svcs, err error) {
			m.analytics.Overview(anyCtx, actor).Return(overview, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			if got := apitest.Data[services.Overview](t, res); got != overview {
				t.Errorf("data = %s", res.Data)
			}
		},
	},
}
