package api

//go:generate go tool mockgen -source=audit.go -destination=mocks/audit.go -package=mocks

import (
	"context"
	"net/http"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/access"
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
	svc  AuditService
	gate *access.Gate
}

// NewAudit returns the audit endpoint.
func NewAudit(svc AuditService, gate *access.Gate) *Audit {
	return &Audit{svc: svc, gate: gate}
}

// Routes registers the endpoint on mux with its access policy.
func (audit *Audit) Routes(mux *http.ServeMux) {
	mux.Handle("GET "+config.APIRoutes.Audit, Route(audit.gate, access.SignedIn, audit.list))
}

func (audit *Audit) list(request *http.Request, caller access.Caller) (services.AuditPage, error) {
	input, err := validate.DecodeQuery[validate.ListAuditLog](request.URL.Query())
	if err != nil {
		return services.AuditPage{}, err
	}
	return audit.svc.List(request.Context(), caller.Actor(), input)
}
