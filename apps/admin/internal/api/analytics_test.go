package api_test

import (
	"testing"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/api/apitest"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
)

var overview = services.Overview{
	TotalUsers: 10, VerifiedUsers: 8, BannedUsers: 1, NewUsers7d: 3, ActiveSessions: 12,
	GeneratedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
}

var analyticsRoutes = map[string]route{
	"GET analytics": {
		method: "GET", path: routes.Analytics,
		expect: func(svcs svcs, err error) {
			svcs.analytics.Overview(anyCtx, actor).Return(overview, err)
		},
		check: func(test *testing.T, res apitest.Response) {
			if got := apitest.Data[services.Overview](test, res); got != overview {
				test.Errorf("data = %s", res.Data)
			}
		},
	},
}
