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

	overview Endpoint[NoInput, services.Overview]
}

// NewAnalytics returns the analytics endpoint.
func NewAnalytics(svc AnalyticsService, gate *access.Gate) *Analytics {
	analytics := &Analytics{svc: svc, gate: gate}
	analytics.overview = Endpoint[NoInput, services.Overview]{"GET", config.APIRoutes.Analytics, access.SignedIn, analytics.getOverview}
	return analytics
}

// Routes registers the endpoint on mux.
func (analytics *Analytics) Routes(mux *http.ServeMux) {
	mux.Handle(analytics.overview.Pattern(), analytics.overview.Serve(analytics.gate))
}

func (analytics *Analytics) getOverview(request *http.Request, caller access.Caller, _ NoInput) (services.Overview, error) {
	return analytics.svc.Overview(request.Context(), caller.Actor())
}
