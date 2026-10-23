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

	list Endpoint[validate.ListAuditLog, services.AuditPage]
}

// NewAudit returns the audit endpoint.
func NewAudit(svc AuditService, gate *access.Gate) *Audit {
	audit := &Audit{svc: svc, gate: gate}
	audit.list = Endpoint[validate.ListAuditLog, services.AuditPage]{"GET", config.APIRoutes.Audit, access.SignedIn, audit.listEntries}
	return audit
}

// Routes registers the endpoint on mux.
func (audit *Audit) Routes(mux *http.ServeMux) {
	mux.Handle(audit.list.Pattern(), audit.list.Serve(audit.gate))
}

func (audit *Audit) listEntries(request *http.Request, caller access.Caller, input validate.ListAuditLog) (services.AuditPage, error) {
	return audit.svc.List(request.Context(), caller.Actor(), input)
}
