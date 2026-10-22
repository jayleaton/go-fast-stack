package ui

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/config"
	"github.com/jayleaton/ai-patterns-go/apps/admin/internal/services"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/i18n"
	"github.com/jayleaton/ai-patterns-go/apps/admin/ui/view"
)

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func date(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return i18n.From(ctx).Date(t)
}

func dateTime(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return i18n.From(ctx).DateTime(t.UTC())
}

func yesNo(ctx context.Context, b bool) string {
	if b {
		return i18n.T(ctx, "common.yes")
	}
	return i18n.T(ctx, "common.no")
}

// UserFilters is the users list query as typed (form values and links keep
// exactly what the admin chose).
type UserFilters struct {
	Search, SearchField, Banned, Role, SortBy, SortDirection, Limit string
}

func userFilters(q url.Values) UserFilters {
	return UserFilters{
		Search:        q.Get("search"),
		SearchField:   q.Get("searchField"),
		Banned:        q.Get("banned"),
		Role:          q.Get("role"),
		SortBy:        q.Get("sortBy"),
		SortDirection: q.Get("sortDirection"),
		Limit:         q.Get("limit"),
	}
}

// url is the users list with these filters at offset.
func (f UserFilters) url(offset int) string {
	v := url.Values{}
	set := func(k, val string) {
		if val != "" {
			v.Set(k, val)
		}
	}
	set("search", f.Search)
	set("searchField", f.SearchField)
	set("banned", f.Banned)
	set("role", f.Role)
	set("sortBy", f.SortBy)
	set("sortDirection", f.SortDirection)
	set("limit", f.Limit)
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	return withQuery(config.AppRoutes.Users, v)
}

func withQuery(path string, v url.Values) string {
	if len(v) == 0 {
		return path
	}
	return path + "?" + v.Encode()
}

// sortLinks: clicking the active column flips its direction; another column
// starts ascending (newest first for the created date). Paging restarts.
func (f UserFilters) sortLinks() SortLinks {
	by := firstNonEmpty(f.SortBy, "createdAt")
	desc := firstNonEmpty(f.SortDirection, "desc") == "desc"
	link := func(field, first string) SortLink {
		g := f
		g.SortBy, g.SortDirection = field, first
		active := by == field
		if active {
			g.SortDirection = "desc"
			if desc {
				g.SortDirection = "asc"
			}
		}
		return SortLink{URL: g.url(0), Active: active, Desc: active && desc}
	}
	return SortLinks{
		Created: link("createdAt", "desc"),
		Email:   link("email", "asc"),
		Name:    link("name", "asc"),
	}
}

// sortMark is the arrow after an active column heading.
func sortMark(l SortLink) string {
	switch {
	case !l.Active:
		return ""
	case l.Desc:
		return " ↓"
	}
	return " ↑"
}

// pager builds the pagination row of a page of count rows starting at offset.
func pager[N int | int64](offset, limit, count int, total N, link func(offset int) string) view.Pager {
	p := view.Pager{Total: int(total)}
	if count > 0 {
		p.From, p.To = offset+1, offset+count
	}
	if offset > 0 {
		p.PrevURL = link(max(0, offset-limit))
	}
	if offset+count < int(total) && count > 0 {
		p.NextURL = link(offset + count)
	}
	return p
}

func pagerSummary(ctx context.Context, p view.Pager) string {
	return i18n.Tf(ctx, "components.pager.showing", strconv.Itoa(p.From), strconv.Itoa(p.To), strconv.Itoa(p.Total))
}

func searchFieldOptions(ctx context.Context) []view.Option {
	return []view.Option{
		{Value: "email", Label: i18n.T(ctx, "common.email")},
		{Value: "name", Label: i18n.T(ctx, "common.name")},
	}
}

func bannedOptions(ctx context.Context) []view.Option {
	return []view.Option{
		{Value: "", Label: i18n.T(ctx, "pages.users.filters.all")},
		{Value: "true", Label: i18n.T(ctx, "pages.users.filters.banned")},
		{Value: "false", Label: i18n.T(ctx, "pages.users.filters.active")},
	}
}

func roleOptions() []view.Option {
	out := make([]view.Option, len(services.Roles))
	for i, r := range services.Roles {
		out[i] = view.Option{Value: r, Label: r}
	}
	return out
}

func roleFilterOptions(ctx context.Context) []view.Option {
	return append([]view.Option{{Value: "", Label: i18n.T(ctx, "pages.users.filters.anyRole")}}, roleOptions()...)
}

// banExpiryOptions are the ban lengths in seconds; "0" is permanent.
func banExpiryOptions(ctx context.Context) []view.Option {
	return []view.Option{
		{Value: "3600", Label: i18n.T(ctx, "pages.user.ban.expiry.hour")},
		{Value: "86400", Label: i18n.T(ctx, "pages.user.ban.expiry.day")},
		{Value: "604800", Label: i18n.T(ctx, "pages.user.ban.expiry.week")},
		{Value: "2592000", Label: i18n.T(ctx, "pages.user.ban.expiry.month")},
		{Value: "0", Label: i18n.T(ctx, "pages.user.ban.expiry.permanent")},
	}
}

// banExpires is when a ban ends, or "permanent".
func banExpires(ctx context.Context, u services.User) string {
	if u.BanExpires == nil {
		return i18n.T(ctx, "pages.user.ban.permanent")
	}
	return dateTime(ctx, *u.BanExpires)
}

// actionKeys label the audit actions; unknown actions show raw.
var actionKeys = []struct{ action, key string }{
	{services.ActionUserBan, "pages.audit.actions.userBan"},
	{services.ActionUserUnban, "pages.audit.actions.userUnban"},
	{services.ActionUserSetRole, "pages.audit.actions.userSetRole"},
	{services.ActionUserRevokeSession, "pages.audit.actions.userRevokeSession"},
	{services.ActionUserRevokeSessions, "pages.audit.actions.userRevokeSessions"},
	{services.ActionAdminInvite, "pages.audit.actions.adminInvite"},
	{services.ActionAdminBootstrap, "pages.audit.actions.adminBootstrap"},
	{services.ActionAdminDisable, "pages.audit.actions.adminDisable"},
	{services.ActionAdminEnable, "pages.audit.actions.adminEnable"},
	{services.ActionAdminRemove, "pages.audit.actions.adminRemove"},
	{services.ActionAdminSignIn, "pages.audit.actions.adminSignIn"},
	{services.ActionAdminSignOut, "pages.audit.actions.adminSignOut"},
	{services.ActionAdminTwoFactorFailed, "pages.audit.actions.adminTwoFactorFailed"},
}

// signInMethodKeys label the "method" detail of ActionAdminSignIn; unknown
// methods show raw.
var signInMethodKeys = map[string]string{
	services.SignInPassword:    "pages.audit.methods.password",
	services.SignInTwoFactor:   "pages.audit.methods.twoFactor",
	services.SignInSetPassword: "pages.audit.methods.setPassword",
}

func actionLabel(ctx context.Context, action string) string {
	for _, a := range actionKeys {
		if a.action == action {
			return i18n.T(ctx, a.key)
		}
	}
	return action
}

func actionOptions(ctx context.Context) []view.Option {
	out := []view.Option{{Value: "", Label: i18n.T(ctx, "pages.audit.allActions")}}
	for _, a := range actionKeys {
		out = append(out, view.Option{Value: a.action, Label: i18n.T(ctx, a.key)})
	}
	return out
}

// AuditFilters is the audit log query as typed.
type AuditFilters struct {
	Action, TargetID, Limit string
}

func auditFilters(q url.Values) AuditFilters {
	return AuditFilters{Action: q.Get("action"), TargetID: q.Get("targetId"), Limit: q.Get("limit")}
}

func (f AuditFilters) url(offset int) string {
	v := url.Values{}
	for k, val := range map[string]string{"action": f.Action, "targetId": f.TargetID, "limit": f.Limit} {
		if val != "" {
			v.Set(k, val)
		}
	}
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
	return withQuery(config.AppRoutes.Audit, v)
}

// auditTarget links user targets to their page.
func auditTarget(e services.AuditEntry) (href, label string) {
	label = firstNonEmpty(e.TargetLabel, e.TargetID)
	if e.TargetType == services.TargetUser && e.TargetID != "" {
		href = config.UserPage(e.TargetID)
	}
	return href, label
}

// auditDetails are the detail lines of an entry: the sign-in method and the
// reason a 2FA code was refused translated, anything else as details does.
func auditDetails(ctx context.Context, e services.AuditEntry) []string {
	if len(e.Details) == 1 {
		if m, ok := e.Details["method"]; ok && e.Action == services.ActionAdminSignIn {
			if key, known := signInMethodKeys[m]; known {
				m = i18n.T(ctx, key)
			}
			return []string{i18n.Tf(ctx, "pages.audit.signInMethod", m)}
		}
		if r, ok := e.Details["reason"]; ok && e.Action == services.ActionAdminTwoFactorFailed {
			if key, known := twoFactorReasonKeys[r]; known {
				r = i18n.T(ctx, key)
			}
			return []string{i18n.Tf(ctx, "pages.audit.reason", r)}
		}
	}
	return details(e.Details)
}

// twoFactorReasonKeys translate the packages/auth error codes a refused 2FA
// code is audited with; other codes show as they are.
var twoFactorReasonKeys = map[string]string{
	"INVALID_CODE":               "pages.audit.reasons.invalidCode",
	"INVALID_BACKUP_CODE":        "pages.audit.reasons.invalidBackupCode",
	"ACCOUNT_TEMPORARILY_LOCKED": "pages.audit.reasons.locked",
	"TOO_MANY_REQUESTS":          "pages.audit.reasons.tooManyRequests",
}

// details renders an entry's details as "key: value" pairs, sorted by key.
func details(d map[string]string) []string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + ": " + d[k]
	}
	return out
}

// otpLink is the otpauth:// URI as a link target; anything else (a value a
// plain post sent back) is not linked.
func otpLink(uri string) string {
	if strings.HasPrefix(uri, "otpauth://") {
		return uri
	}
	return ""
}

// groupSecret splits a TOTP secret into groups of four for manual entry.
func groupSecret(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func backupSignals(on bool) string {
	return fmt.Sprintf("{_backup: %t}", on)
}

func codeInputmode(backup bool) string {
	if backup {
		return "text"
	}
	return "numeric"
}
