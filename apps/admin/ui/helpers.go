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

func orDash(text string) string {
	if text == "" {
		return "—"
	}
	return text
}

func deref(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

func firstNonEmpty(vals ...string) string {
	for _, value := range vals {
		if value != "" {
			return value
		}
	}
	return ""
}

func date(ctx context.Context, moment time.Time) string {
	if moment.IsZero() {
		return "—"
	}
	return i18n.From(ctx).Date(moment)
}

func dateTime(ctx context.Context, moment time.Time) string {
	if moment.IsZero() {
		return "—"
	}
	return i18n.From(ctx).DateTime(moment.UTC())
}

func yesNo(ctx context.Context, flag bool) string {
	if flag {
		return i18n.Translate(ctx, "common.yes")
	}
	return i18n.Translate(ctx, "common.no")
}

// UserFilters is the users list query as typed (form values and links keep
// exactly what the admin chose).
type UserFilters struct {
	Search, SearchField, Banned, Role, SortBy, SortDirection, Limit string
}

func userFilters(values url.Values) UserFilters {
	return UserFilters{
		Search:        values.Get("search"),
		SearchField:   values.Get("searchField"),
		Banned:        values.Get("banned"),
		Role:          values.Get("role"),
		SortBy:        values.Get("sortBy"),
		SortDirection: values.Get("sortDirection"),
		Limit:         values.Get("limit"),
	}
}

// url is the users list with these filters at offset.
func (filters UserFilters) url(offset int) string {
	values := url.Values{}
	set := func(key, val string) {
		if val != "" {
			values.Set(key, val)
		}
	}
	set("search", filters.Search)
	set("searchField", filters.SearchField)
	set("banned", filters.Banned)
	set("role", filters.Role)
	set("sortBy", filters.SortBy)
	set("sortDirection", filters.SortDirection)
	set("limit", filters.Limit)
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	return withQuery(config.AppRoutes.Users, values)
}

func withQuery(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

// sortLinks: clicking the active column flips its direction; another column
// starts ascending (newest first for the created date). Paging restarts.
func (filters UserFilters) sortLinks() SortLinks {
	by := firstNonEmpty(filters.SortBy, "createdAt")
	desc := firstNonEmpty(filters.SortDirection, "desc") == "desc"
	link := func(field, first string) SortLink {
		userFilters := filters
		userFilters.SortBy, userFilters.SortDirection = field, first
		active := by == field
		if active {
			userFilters.SortDirection = "desc"
			if desc {
				userFilters.SortDirection = "asc"
			}
		}
		return SortLink{URL: userFilters.url(0), Active: active, Desc: active && desc}
	}
	return SortLinks{
		Created: link("createdAt", "desc"),
		Email:   link("email", "asc"),
		Name:    link("name", "asc"),
	}
}

// sortMark is the arrow after an active column heading.
func sortMark(link SortLink) string {
	switch {
	case !link.Active:
		return ""
	case link.Desc:
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
func pager[Value int | int64](offset, limit, count int, total Value, link func(offset int) string) Pager {
	pager := Pager{Total: int(total)}
	if count > 0 {
		pager.From, pager.To = offset+1, offset+count
	}
	if offset > 0 {
		pager.PrevURL = link(max(0, offset-limit))
	}
	if offset+count < int(total) && count > 0 {
		pager.NextURL = link(offset + count)
	}
	return pager
}

// pagination is p as the translated uikit pagination row.
func pagination(ctx context.Context, pager Pager) uikit.Pager {
	return uikit.Pager{
		Summary:   i18n.Tf(ctx, "components.pager.showing", strconv.Itoa(pager.From), strconv.Itoa(pager.To), strconv.Itoa(pager.Total)),
		PrevURL:   pager.PrevURL,
		NextURL:   pager.NextURL,
		PrevLabel: i18n.Translate(ctx, "components.pager.previous"),
		NextLabel: i18n.Translate(ctx, "components.pager.next"),
		Label:     i18n.Translate(ctx, "components.pager.label"),
	}
}

// filtered reports whether the users list is narrowed by a search or filter.
func (filters UserFilters) filtered() bool {
	return filters.Search != "" || filters.Banned != "" || filters.Role != ""
}

// sortOrder is the aria-sort value of a column heading ("" when unsorted).
func sortOrder(link SortLink) string {
	switch {
	case !link.Active:
		return ""
	case link.Desc:
		return "descending"
	}
	return "ascending"
}

func searchFieldOptions(ctx context.Context) []uikit.Option {
	return []uikit.Option{
		{Value: "email", Label: i18n.Translate(ctx, "common.email")},
		{Value: "name", Label: i18n.Translate(ctx, "common.name")},
	}
}

func bannedOptions(ctx context.Context) []uikit.Option {
	return []uikit.Option{
		{Value: "", Label: i18n.Translate(ctx, "pages.users.filters.all")},
		{Value: "true", Label: i18n.Translate(ctx, "pages.users.filters.banned")},
		{Value: "false", Label: i18n.Translate(ctx, "pages.users.filters.active")},
	}
}

func roleOptions() []uikit.Option {
	out := make([]uikit.Option, len(apiclient.Roles))
	for index, role := range apiclient.Roles {
		out[index] = uikit.Option{Value: role, Label: role}
	}
	return out
}

func roleFilterOptions(ctx context.Context) []uikit.Option {
	return append([]uikit.Option{{Value: "", Label: i18n.Translate(ctx, "pages.users.filters.anyRole")}}, roleOptions()...)
}

// banExpiryOptions are the ban lengths in seconds; "0" is permanent.
func banExpiryOptions(ctx context.Context) []uikit.Option {
	return []uikit.Option{
		{Value: "3600", Label: i18n.Translate(ctx, "pages.user.ban.expiry.hour")},
		{Value: "86400", Label: i18n.Translate(ctx, "pages.user.ban.expiry.day")},
		{Value: "604800", Label: i18n.Translate(ctx, "pages.user.ban.expiry.week")},
		{Value: "2592000", Label: i18n.Translate(ctx, "pages.user.ban.expiry.month")},
		{Value: "0", Label: i18n.Translate(ctx, "pages.user.ban.expiry.permanent")},
	}
}

// banExpires is when a ban ends, or "permanent".
func banExpires(ctx context.Context, user apiclient.User) string {
	if user.BanExpires == nil {
		return i18n.Translate(ctx, "pages.user.ban.permanent")
	}
	return dateTime(ctx, *user.BanExpires)
}

// banLabel is the ban button: ban, or update the ban of a banned user.
func banLabel(ctx context.Context, user apiclient.User) string {
	if user.Banned {
		return i18n.Translate(ctx, "pages.user.ban.update")
	}
	return i18n.Translate(ctx, "pages.user.ban.submit")
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
	for _, actionKey := range actionKeys {
		if actionKey.action == action {
			return i18n.Translate(ctx, actionKey.key)
		}
	}
	return action
}

func actionOptions(ctx context.Context) []uikit.Option {
	out := []uikit.Option{{Value: "", Label: i18n.Translate(ctx, "pages.audit.allActions")}}
	for _, actionKey := range actionKeys {
		out = append(out, uikit.Option{Value: actionKey.action, Label: i18n.Translate(ctx, actionKey.key)})
	}
	return out
}

// AuditFilters is the audit log query as typed.
type AuditFilters struct {
	Action, TargetID, Limit string
}

func auditFilters(values url.Values) AuditFilters {
	return AuditFilters{Action: values.Get("action"), TargetID: values.Get("targetId"), Limit: values.Get("limit")}
}

func (filters AuditFilters) url(offset int) string {
	values := url.Values{}
	for key, val := range map[string]string{"action": filters.Action, "targetId": filters.TargetID, "limit": filters.Limit} {
		if val != "" {
			values.Set(key, val)
		}
	}
	if offset > 0 {
		values.Set("offset", strconv.Itoa(offset))
	}
	return withQuery(config.AppRoutes.Audit, values)
}

// auditTarget links user targets to their page.
func auditTarget(entry apiclient.AuditEntry) (href, label string) {
	label = firstNonEmpty(entry.TargetLabel, entry.TargetID)
	if entry.TargetType == apiclient.TargetUser && entry.TargetID != "" {
		href = config.UserPage(entry.TargetID)
	}
	return href, label
}

// auditDetails are the detail lines of an entry: the sign-in method and the
// reason a 2FA code was refused translated, anything else as details does.
func auditDetails(ctx context.Context, entry apiclient.AuditEntry) []string {
	if len(entry.Details) == 1 {
		if message, ok := entry.Details["method"]; ok && entry.Action == apiclient.ActionAdminSignIn {
			if key, known := signInMethodKeys[message]; known {
				message = i18n.Translate(ctx, key)
			}
			return []string{i18n.Tf(ctx, "pages.audit.signInMethod", message)}
		}
		if raw, ok := entry.Details["reason"]; ok && entry.Action == apiclient.ActionAdminTwoFactorFailed {
			if key, known := twoFactorReasonKeys[raw]; known {
				raw = i18n.Translate(ctx, key)
			}
			return []string{i18n.Tf(ctx, "pages.audit.reason", raw)}
		}
	}
	return details(entry.Details)
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
func auditStatus(ctx context.Context, entry apiclient.AuditEntry) AuditStatus {
	status := AuditStatus{Tone: uikit.Neutral, Label: orDash(entry.Status), Reason: auditReason(ctx, entry.Reason), Reconciled: entry.Reconciled}
	if item, ok := auditStatusKeys[entry.Status]; ok {
		status.Tone, status.Label = item.tone, i18n.Translate(ctx, item.key)
	}
	if entry.Status == apiclient.StatusPending {
		status.Hint = i18n.Translate(ctx, "pages.audit.statuses.pendingHint")
	}
	if key, ok := reconciledReasonKeys[entry.Status]; ok && entry.Reconciled {
		status.Reason = i18n.Translate(ctx, key)
	}
	return status
}

// auditReason translates a recorded failure reason that is the message of a
// service error the UI knows; anything else shows as recorded.
func auditReason(ctx context.Context, reason string) string {
	if reason == apiclient.ReasonInternal {
		return i18n.Translate(ctx, "pages.audit.internalReason")
	}
	for _, errorKey := range errorKeys {
		var se *apiclient.ServiceError
		if errors.As(errorKey.err, &se) && se.Message == reason {
			return i18n.Translate(ctx, errorKey.key)
		}
	}
	return reason
}

// details renders an entry's details as "key: value" pairs, sorted by key.
func details(lookup map[string]string) []string {
	keys := make([]string, 0, len(lookup))
	for key := range lookup {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]string, len(keys))
	for index, key := range keys {
		out[index] = key + ": " + lookup[key]
	}
	return out
}

// statusTone is the alert tone of an action outcome.
func statusTone(status Status) uikit.Tone {
	if status.Error {
		return uikit.Error
	}
	return uikit.Success
}

// analyticsStats are the overview's figures as stat tiles, in reading order.
func analyticsStats(ctx context.Context, overview apiclient.Overview) []uikit.Stat {
	stat := func(key, id string, value int64) uikit.Stat {
		return uikit.Stat{Label: i18n.Translate(ctx, key), Value: strconv.FormatInt(value, 10), TestID: id}
	}
	return []uikit.Stat{
		stat("pages.analytics.totalUsers", "stat-total-users", overview.TotalUsers),
		stat("pages.analytics.newUsers7d", "stat-new-users", overview.NewUsers7d),
		stat("pages.analytics.verifiedUsers", "stat-verified-users", overview.VerifiedUsers),
		stat("pages.analytics.bannedUsers", "stat-banned-users", overview.BannedUsers),
		stat("pages.analytics.activeSessions", "stat-active-sessions", overview.ActiveSessions),
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
func groupSecret(text string) string {
	var builder strings.Builder
	for index, char := range text {
		if index > 0 && index%4 == 0 {
			builder.WriteByte(' ')
		}
		builder.WriteRune(char)
	}
	return builder.String()
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
