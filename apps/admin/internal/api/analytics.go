package api

//go:generate go tool mockgen -source=analytics.go -destination=mocks/analytics.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
)

// AnalyticsService is the port this endpoint calls (*services.AnalyticsService).
type AnalyticsService interface {
	Overview(ctx context.Context, actor services.Actor) (services.Overview, error)
}

var _ AnalyticsService = (*services.AnalyticsService)(nil)

// Analytics serves the headline numbers under /api/admin/v1/analytics.
type Analytics struct {
	svc    AnalyticsService
	caller CallerFrom
}

// NewAnalytics returns the analytics endpoint.
func NewAnalytics(svc AnalyticsService, caller CallerFrom) *Analytics {
	return &Analytics{svc: svc, caller: caller}
}

// Routes registers the endpoint on mux.
func (a *Analytics) Routes(mux *http.ServeMux) {
	mux.Handle("GET "+config.APIRoutes.Analytics, Authenticated(a.caller, a.overview))
}

func (a *Analytics) overview(r *http.Request, c Caller) (services.Overview, error) {
	return a.svc.Overview(r.Context(), c.Actor())
}
