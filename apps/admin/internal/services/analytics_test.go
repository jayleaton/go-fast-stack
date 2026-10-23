package services_test

import (
	"testing"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
)

func TestOverview(test *testing.T) {
	test.Parallel()
	ports := newPorts(test)
	svc := services.NewAnalyticsService(ports.analytics, services.Policy{RequireTwoFactor: true})
	want := services.Overview{TotalUsers: 10, VerifiedUsers: 8, BannedUsers: 1, NewUsers7d: 2, ActiveSessions: 5, GeneratedAt: time.Unix(1700000000, 0)}

	ports.analytics.EXPECT().Overview(ctx).Return(want, nil)
	if got, err := svc.Overview(ctx, grace); err != nil || got != want {
		test.Fatalf("Overview = %+v, %v", got, err)
	}

	ports.analytics.EXPECT().Overview(ctx).Return(services.Overview{}, errDB)
	_, err := svc.Overview(ctx, grace)
	wantInternal(test, err, errDB)
}
