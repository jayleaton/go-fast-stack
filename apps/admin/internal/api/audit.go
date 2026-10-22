package api

//go:generate go tool mockgen -source=audit.go -destination=mocks/audit.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/services"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
)

// AuditService is the port this endpoint calls (*services.AuditService).
type AuditService interface {
	List(ctx context.Context, actor services.Actor, in validate.ListAuditLog) (services.AuditPage, error)
}

var _ AuditService = (*services.AuditService)(nil)

// Audit serves the audit log under /api/admin/v1/audit.
type Audit struct {
	svc    AuditService
	caller CallerFrom
}

// NewAudit returns the audit endpoint.
func NewAudit(svc AuditService, caller CallerFrom) *Audit {
	return &Audit{svc: svc, caller: caller}
}

// Routes registers the endpoint on mux.
func (a *Audit) Routes(mux *http.ServeMux) {
	mux.Handle("GET "+config.APIRoutes.Audit, Authenticated(a.caller, a.list))
}

func (a *Audit) list(r *http.Request, c Caller) (services.AuditPage, error) {
	in, err := validate.DecodeQuery[validate.ListAuditLog](r.URL.Query())
	if err != nil {
		return services.AuditPage{}, err
	}
	return a.svc.List(r.Context(), c.Actor(), in)
}
