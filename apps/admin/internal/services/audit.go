package services

//go:generate go tool mockgen -source=audit.go -destination=mocks/audit.go -package=mocks

import (
	"context"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/validate"
	"github.com/jayleaton/go-fast-stack/packages/database/adminrepo/model"
)

// The audit models, re-exported so transports never import adminrepo.
type (
	NewAuditEntry = model.NewAuditEntry
	AuditEntry    = model.AuditEntry
	AuditQuery    = model.AuditQuery
	AuditPage     = model.AuditPage
	AuditOutcome  = model.AuditOutcome
)

// Audit statuses (see model.Status*).
const (
	StatusPending   = model.StatusPending
	StatusSucceeded = model.StatusSucceeded
	StatusFailed    = model.StatusFailed
	StatusUnknown   = model.StatusUnknown
)

// Audit targets.
const (
	TargetUser  = model.TargetUser
	TargetAdmin = model.TargetAdmin
)

// AuditLog is the port to admin_audit_log in the admin database
// (adminrepo.AuditLog). Entries are append-only.
type AuditLog interface {
	// Append records a completed fact (no outcome follows).
	Append(ctx context.Context, e NewAuditEntry) error
	// Begin records a mutation's intent (pending) before it runs.
	Begin(ctx context.Context, e NewAuditEntry) (id int64, err error)
	// Resolve appends the outcome of a pending entry; the first one wins.
	Resolve(ctx context.Context, id int64, o AuditOutcome) error
	// Pending lists up to limit intents older than before without an outcome.
	Pending(ctx context.Context, before time.Time, limit int) ([]AuditEntry, error)
	List(ctx context.Context, q AuditQuery) (AuditPage, error)
}

// AuditService reads the audit log. Writes happen inside the services whose
// actions they record.
type AuditService struct {
	log    AuditLog
	policy Policy
}

// NewAuditService returns an AuditService over its port.
func NewAuditService(log AuditLog, policy Policy) *AuditService {
	return &AuditService{log: log, policy: policy}
}

// List returns one page of the audit log, newest first.
func (s *AuditService) List(ctx context.Context, actor Actor, in validate.ListAuditLog) (AuditPage, error) {
	if err := s.policy.authorize(actor); err != nil {
		return AuditPage{}, err
	}
	page, err := s.log.List(ctx, AuditQuery{Action: in.Action, TargetID: in.TargetID, Limit: in.Limit, Offset: in.Offset})
	if page.Entries == nil {
		page.Entries = []AuditEntry{}
	}
	return page, err
}
