package api_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/api/apitest"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/validate"
)

var banEntry = services.AuditEntry{
	ID: 7, ActorID: "a1", ActorEmail: grace.Email, Action: services.ActionUserBan,
	TargetType: services.TargetUser, TargetID: "u2", TargetLabel: "ada@example.com",
	Details: map[string]string{"reason": "spam"}, CreatedAt: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
}

var auditRoutes = map[string]route{
	"GET audit": {
		method: "GET", path: r.Audit, invalidPath: r.Audit + "?offset=-1", field: "offset",
		expect: func(m svcs, err error) {
			m.audit.List(anyCtx, actor, validate.ListAuditLog{Limit: validate.DefaultLimit}).Return(services.AuditPage{Entries: []services.AuditEntry{banEntry}, Total: 1, Limit: 25}, err)
		},
		check: func(t *testing.T, res apitest.Response) {
			got := apitest.Data[services.AuditPage](t, res)
			if len(got.Entries) != 1 || got.Total != 1 || got.Limit != 25 {
				t.Fatalf("data = %s", res.Data)
			}
			e := got.Entries[0]
			if e.ID != 7 || e.Action != "user.ban" || e.TargetLabel != banEntry.TargetLabel || e.Details["reason"] != "spam" || !e.CreatedAt.Equal(banEntry.CreatedAt) {
				t.Errorf("entry = %+v", e)
			}
		},
	},
}

// TestListAuditLogQuery: filters are trimmed, the page size defaults, and the
// bounds are inclusive.
func TestListAuditLogQuery(t *testing.T) {
	cases := []struct {
		q    url.Values
		want validate.ListAuditLog
	}{
		{url.Values{"limit": {"0"}}, validate.ListAuditLog{Limit: 25}},
		{
			url.Values{"action": {" user.ban "}, "targetId": {" u2 "}, "limit": {"100"}, "offset": {"10"}},
			validate.ListAuditLog{Action: "user.ban", TargetID: "u2", Limit: 100, Offset: 10},
		},
		{
			url.Values{"action": {strings.Repeat("a", 64)}, "targetId": {strings.Repeat("t", 255)}, "limit": {"1"}},
			validate.ListAuditLog{Action: strings.Repeat("a", 64), TargetID: strings.Repeat("t", 255), Limit: 1},
		},
	}
	for _, c := range cases {
		wantOK(t, "GET", r.Audit+"?"+c.q.Encode(), "", func(m svcs) {
			m.audit.List(anyCtx, actor, c.want)
		})
	}
}

// TestListAuditLogInvalid covers every ListAuditLog rule.
func TestListAuditLogInvalid(t *testing.T) {
	limit := map[string]string{"limit": "must be 1-100"}
	cases := []struct {
		q    string
		want map[string]string
	}{
		{"limit=1000", limit},
		{"limit=101", limit},
		{"limit=-5", limit},
		{"limit=ten", map[string]string{"limit": "must be a whole number"}},
		{"offset=-1", map[string]string{"offset": "must be 0 or more"}},
		{"offset=y&limit=z", map[string]string{"offset": "must be a whole number", "limit": "must be a whole number"}},
		{"action=" + strings.Repeat("a", 65), map[string]string{"action": "must be at most 64 characters"}},
		{"targetId=" + strings.Repeat("t", 256), map[string]string{"targetId": "must be at most 255 characters"}},
		{"action=" + strings.Repeat("a", 65) + "&targetId=" + strings.Repeat("t", 256) + "&limit=101&offset=-1", map[string]string{
			"action": "must be at most 64 characters", "targetId": "must be at most 255 characters", "limit": "must be 1-100", "offset": "must be 0 or more",
		}},
	}
	for _, c := range cases {
		wantInvalid(t, "GET", r.Audit+"?"+c.q, "", c.want)
	}
}
