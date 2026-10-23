package api

//go:generate go tool mockgen -source=analytics.go -destination=mocks/analytics.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
)

// AnalyticsService is the port this endpoint calls (*services.AnalyticsService).
type AnalyticsService interface {
	Overview(ctx context.Context, actor services.Actor) (services.Overview, error)
}

var _ AnalyticsService = (*services.AnalyticsService)(nil)

// Analytics serves the headline numbers under /api/admin/v1/analytics.
type Analytics struct {
	svc  AnalyticsService
	gate *access.Gate
}

// NewAnalytics returns the analytics endpoint.
func NewAnalytics(svc AnalyticsService, gate *access.Gate) *Analytics {
	return &Analytics{svc: svc, gate: gate}
}

// Routes registers the endpoint on mux with its access policy.
func (analytics *Analytics) Routes(mux *http.ServeMux) {
	mux.Handle("GET "+config.APIRoutes.Analytics, Route(analytics.gate, access.SignedIn, analytics.overview))
}

func (analytics *Analytics) overview(request *http.Request, caller access.Caller) (services.Overview, error) {
	return analytics.svc.Overview(request.Context(), caller.Actor())
}
