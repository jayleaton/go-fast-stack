package ui

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/apps/admin/internal/apiclient"
	"github.com/jayleaton/go-fast-stack/apps/admin/internal/config"
	"github.com/jayleaton/go-fast-stack/apps/admin/ui/i18n"
	"github.com/jayleaton/go-fast-stack/packages/uikit"
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

// Pager is the position of a list page: rows From-To of Total, with the
// previous and next page links (empty: no such page).
type Pager struct {
	From, To, Total  int
	PrevURL, NextURL string
}

// pager builds the pagination row of a page of count rows starting at offset.
func pager[N int | int64](offset, limit, count int, total N, link func(offset int) string) Pager {
	p := Pager{Total: int(total)}
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

// pagination is p as the translated uikit pagination row.
func pagination(ctx context.Context, p Pager) uikit.Pager {
	return uikit.Pager{
		Summary:   i18n.Tf(ctx, "components.pager.showing", strconv.Itoa(p.From), strconv.Itoa(p.To), strconv.Itoa(p.Total)),
		PrevURL:   p.PrevURL,
		NextURL:   p.NextURL,
		PrevLabel: i18n.T(ctx, "components.pager.previous"),
		NextLabel: i18n.T(ctx, "components.pager.next"),
		Label:     i18n.T(ctx, "components.pager.label"),
	}
}

// filtered reports whether the users list is narrowed by a search or filter.
func (f UserFilters) filtered() bool {
	return f.Search != "" || f.Banned != "" || f.Role != ""
}

// sortOrder is the aria-sort value of a column heading ("" when unsorted).
func sortOrder(l SortLink) string {
	switch {
	case !l.Active:
		return ""
	case l.Desc:
		return "descending"
	}
	return "ascending"
}

func searchFieldOptions(ctx context.Context) []uikit.Option {
	return []uikit.Option{
		{Value: "email", Label: i18n.T(ctx, "common.email")},
		{Value: "name", Label: i18n.T(ctx, "common.name")},
	}
}

func bannedOptions(ctx context.Context) []uikit.Option {
	return []uikit.Option{
		{Value: "", Label: i18n.T(ctx, "pages.users.filters.all")},
		{Value: "true", Label: i18n.T(ctx, "pages.users.filters.banned")},
		{Value: "false", Label: i18n.T(ctx, "pages.users.filters.active")},
	}
}

func roleOptions() []uikit.Option {
	out := make([]uikit.Option, len(apiclient.Roles))
	for i, r := range apiclient.Roles {
		out[i] = uikit.Option{Value: r, Label: r}
	}
	return out
}

func roleFilterOptions(ctx context.Context) []uikit.Option {
	return append([]uikit.Option{{Value: "", Label: i18n.T(ctx, "pages.users.filters.anyRole")}}, roleOptions()...)
}

// banExpiryOptions are the ban lengths in seconds; "0" is permanent.
func banExpiryOptions(ctx context.Context) []uikit.Option {
	return []uikit.Option{
		{Value: "3600", Label: i18n.T(ctx, "pages.user.ban.expiry.hour")},
		{Value: "86400", Label: i18n.T(ctx, "pages.user.ban.expiry.day")},
		{Value: "604800", Label: i18n.T(ctx, "pages.user.ban.expiry.week")},
		{Value: "2592000", Label: i18n.T(ctx, "pages.user.ban.expiry.month")},
		{Value: "0", Label: i18n.T(ctx, "pages.user.ban.expiry.permanent")},
	}
}

// banExpires is when a ban ends, or "permanent".
func banExpires(ctx context.Context, u apiclient.User) string {
	if u.BanExpires == nil {
		return i18n.T(ctx, "pages.user.ban.permanent")
	}
	return dateTime(ctx, *u.BanExpires)
}

// banLabel is the ban button: ban, or update the ban of a banned user.
func banLabel(ctx context.Context, u apiclient.User) string {
	if u.Banned {
		return i18n.T(ctx, "pages.user.ban.update")
	}
	return i18n.T(ctx, "pages.user.ban.submit")
}

// actionKeys label the audit actions; unknown actions show raw.
var actionKeys = []struct{ action, key string }{
	{apiclient.ActionUserBan, "pages.audit.actions.userBan"},
	{apiclient.ActionUserUnban, "pages.audit.actions.userUnban"},
	{apiclient.ActionUserSetRole, "pages.audit.actions.userSetRole"},
	{apiclient.ActionUserRevokeSession, "pages.audit.actions.userRevokeSession"},
	{apiclient.ActionUserRevokeSessions, "pages.audit.actions.userRevokeSessions"},
	{apiclient.ActionAdminInvite, "pages.audit.actions.adminInvite"},
	{apiclient.ActionAdminBootstrap, "pages.audit.actions.adminBootstrap"},
	{apiclient.ActionAdminDisable, "pages.audit.actions.adminDisable"},
	{apiclient.ActionAdminEnable, "pages.audit.actions.adminEnable"},
	{apiclient.ActionAdminRemove, "pages.audit.actions.adminRemove"},
	{apiclient.ActionAdminPasswordReset, "pages.audit.actions.adminPasswordReset"},
	{apiclient.ActionAdminResetTwoFactor, "pages.audit.actions.adminResetTwoFactor"},
	{apiclient.ActionAdminSignIn, "pages.audit.actions.adminSignIn"},
	{apiclient.ActionAdminSignOut, "pages.audit.actions.adminSignOut"},
	{apiclient.ActionAdminTwoFactorFailed, "pages.audit.actions.adminTwoFactorFailed"},
	{apiclient.ActionAdminBackupCodeUsed, "pages.audit.actions.adminBackupCodeUsed"},
}

// signInMethodKeys label the "method" detail of ActionAdminSignIn; unknown
// methods show raw.
var signInMethodKeys = map[string]string{
	apiclient.SignInPassword:    "pages.audit.methods.password",
	apiclient.SignInTwoFactor:   "pages.audit.methods.twoFactor",
	apiclient.SignInSetPassword: "pages.audit.methods.setPassword",
}

func actionLabel(ctx context.Context, action string) string {
	for _, a := range actionKeys {
		if a.action == action {
			return i18n.T(ctx, a.key)
		}
	}
	return action
}

func actionOptions(ctx context.Context) []uikit.Option {
	out := []uikit.Option{{Value: "", Label: i18n.T(ctx, "pages.audit.allActions")}}
	for _, a := range actionKeys {
		out = append(out, uikit.Option{Value: a.action, Label: i18n.T(ctx, a.key)})
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
func auditTarget(e apiclient.AuditEntry) (href, label string) {
	label = firstNonEmpty(e.TargetLabel, e.TargetID)
	if e.TargetType == apiclient.TargetUser && e.TargetID != "" {
		href = config.UserPage(e.TargetID)
	}
	return href, label
}

// auditDetails are the detail lines of an entry: the sign-in method and the
// reason a 2FA code was refused translated, anything else as details does.
func auditDetails(ctx context.Context, e apiclient.AuditEntry) []string {
	if len(e.Details) == 1 {
		if m, ok := e.Details["method"]; ok && e.Action == apiclient.ActionAdminSignIn {
			if key, known := signInMethodKeys[m]; known {
				m = i18n.T(ctx, key)
			}
			return []string{i18n.Tf(ctx, "pages.audit.signInMethod", m)}
		}
		if r, ok := e.Details["reason"]; ok && e.Action == apiclient.ActionAdminTwoFactorFailed {
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

// AuditStatus is an entry's outcome as the audit log shows it (translated).
type AuditStatus struct {
	Tone       uikit.Tone
	Label      string
	Hint       string // what the status means, where it needs saying
	Reason     string // why it failed, or how it was reconciled
	Reconciled bool
}

// auditStatusKeys label the audit statuses with a badge tone; unknown
// statuses show raw.
var auditStatusKeys = map[string]struct {
	tone uikit.Tone
	key  string
}{
	apiclient.StatusSucceeded: {uikit.Success, "pages.audit.statuses.succeeded"},
	apiclient.StatusFailed:    {uikit.Error, "pages.audit.statuses.failed"},
	apiclient.StatusPending:   {uikit.Warning, "pages.audit.statuses.pending"},
	apiclient.StatusUnknown:   {uikit.Neutral, "pages.audit.statuses.unknown"},
}

// reconciledReasonKeys translate the reconciler's reasons, which follow from
// the status it resolved an entry to.
var reconciledReasonKeys = map[string]string{
	apiclient.StatusSucceeded: "pages.audit.reconciledReasons.stateMatches",
	apiclient.StatusFailed:    "pages.audit.reconciledReasons.stateDiffers",
	apiclient.StatusUnknown:   "pages.audit.reconciledReasons.undecidable",
}

// auditStatus is the badge, hint and reason of an entry. A pending entry's
// outcome was never recorded: the change may or may not have been applied.
func auditStatus(ctx context.Context, e apiclient.AuditEntry) AuditStatus {
	s := AuditStatus{Tone: uikit.Neutral, Label: orDash(e.Status), Reason: auditReason(ctx, e.Reason), Reconciled: e.Reconciled}
	if k, ok := auditStatusKeys[e.Status]; ok {
		s.Tone, s.Label = k.tone, i18n.T(ctx, k.key)
	}
	if e.Status == apiclient.StatusPending {
		s.Hint = i18n.T(ctx, "pages.audit.statuses.pendingHint")
	}
	if key, ok := reconciledReasonKeys[e.Status]; ok && e.Reconciled {
		s.Reason = i18n.T(ctx, key)
	}
	return s
}

// auditReason translates a recorded failure reason that is the message of a
// service error the UI knows; anything else shows as recorded.
func auditReason(ctx context.Context, reason string) string {
	if reason == apiclient.ReasonInternal {
		return i18n.T(ctx, "pages.audit.internalReason")
	}
	for _, k := range errorKeys {
		var se *apiclient.ServiceError
		if errors.As(k.err, &se) && se.Message == reason {
			return i18n.T(ctx, k.key)
		}
	}
	return reason
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

// statusTone is the alert tone of an action outcome.
func statusTone(s Status) uikit.Tone {
	if s.Error {
		return uikit.Error
	}
	return uikit.Success
}

// analyticsStats are the overview's figures as stat tiles, in reading order.
func analyticsStats(ctx context.Context, o apiclient.Overview) []uikit.Stat {
	stat := func(key, id string, v int64) uikit.Stat {
		return uikit.Stat{Label: i18n.T(ctx, key), Value: strconv.FormatInt(v, 10), TestID: id}
	}
	return []uikit.Stat{
		stat("pages.analytics.totalUsers", "stat-total-users", o.TotalUsers),
		stat("pages.analytics.newUsers7d", "stat-new-users", o.NewUsers7d),
		stat("pages.analytics.verifiedUsers", "stat-verified-users", o.VerifiedUsers),
		stat("pages.analytics.bannedUsers", "stat-banned-users", o.BannedUsers),
		stat("pages.analytics.activeSessions", "stat-active-sessions", o.ActiveSessions),
	}
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
