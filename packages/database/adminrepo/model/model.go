// Package model holds the plain data types the admin repositories return. It
// has no database dependencies, so apps/admin services can expose these types
// to their transports. Only apps/admin may import it (depguard).
package model

import (
	"errors"
	"time"
)

// ErrNotFound is returned by admin repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// Audit targets.
const (
	TargetUser  = "user"  // a user of the main app
	TargetAdmin = "admin" // an admin of this dashboard
)

// NewAuditEntry is one admin action to record. Details are short display
// values (ban reason, new role...), never secrets.
type NewAuditEntry struct {
	ActorID     string
	ActorEmail  string
	Action      string // "user.ban", "admin.invite", ...
	TargetType  string // TargetUser or TargetAdmin
	TargetID    string
	TargetLabel string // the target's email when the entry was written
	Details     map[string]string
}

// Audit statuses. A mutation's entry is written pending before it runs and
// resolved afterwards; an entry that records a fact (a sign-in) is succeeded.
const (
	StatusPending   = "pending"   // attempted; the outcome was never recorded (shown as "outcome unknown")
	StatusSucceeded = "succeeded" // the change was applied
	StatusFailed    = "failed"    // the change was refused or failed; nothing applied
	StatusUnknown   = "unknown"   // the reconciler could not tell from the current state
)

// AuditOutcome resolves a pending entry. Reason is safe to show (a service
// error message), never a raw database or driver error.
type AuditOutcome struct {
	Status     string // StatusSucceeded, StatusFailed or StatusUnknown
	Reason     string
	Reconciled bool // written by the reconciler from the current state
}

// AuditEntry is a recorded admin action (admin_audit_log row).
type AuditEntry struct {
	ID          int64             `json:"id"`
	ActorID     string            `json:"actorId"`
	ActorEmail  string            `json:"actorEmail"`
	Action      string            `json:"action"`
	TargetType  string            `json:"targetType"`
	TargetID    string            `json:"targetId"`
	TargetLabel string            `json:"targetLabel"`
	Details     map[string]string `json:"details"`
	CreatedAt   time.Time         `json:"createdAt"`
	Status      string            `json:"status"`           // one of the Status* values
	Reason      string            `json:"reason,omitempty"` // why it failed (or how it was reconciled)
	Reconciled  bool              `json:"reconciled"`
}

// AuditQuery filters and pages the audit log, newest first.
type AuditQuery struct {
	Action   string // optional
	TargetID string // optional
	Limit    int
	Offset   int
}

// AuditPage is one page of the audit log.
type AuditPage struct {
	Entries []AuditEntry `json:"entries"`
	Total   int64        `json:"total"`
	Limit   int          `json:"limit"`
	Offset  int          `json:"offset"`
}

// Overview is the analytics placeholder: headline counts over the main app's
// users and sessions. Swap the repository behind services.Analytics for a
// real analytics source later.
type Overview struct {
	TotalUsers     int64     `json:"totalUsers"`
	VerifiedUsers  int64     `json:"verifiedUsers"`
	BannedUsers    int64     `json:"bannedUsers"`
	NewUsers7d     int64     `json:"newUsers7d"`
	ActiveSessions int64     `json:"activeSessions"`
	GeneratedAt    time.Time `json:"generatedAt"`
}
