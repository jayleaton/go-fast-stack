package validate

import (
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Page sizes for every admin list.
const (
	DefaultLimit = 25
	MaxLimit     = 100
)

// Length limits; each error message states the same number.
const (
	maxNameLength        = 255 // a display name, in characters
	maxEmailLength       = 255 // an email address, in bytes
	maxSearchLength      = 255 // a user search, in characters
	maxBanReasonLength   = 500 // a ban reason, in characters
	maxRoleLength        = 32  // a role name, in bytes
	maxAuditActionLength = 64  // an audit action filter, in bytes
	maxAuditTargetLength = 255 // an audit target id filter, in bytes
)

// ListUsers is the query of GET /users (Better Auth list-users names, minus
// the operators). Rules that need state stay in the service.
type ListUsers struct {
	Search        string // case-insensitive contains, on SearchField
	SearchField   string // "email" (default) or "name"
	Role          string // filter, optional
	Banned        *bool  // filter, optional
	SortBy        string // "createdAt" (default), "email" or "name"
	SortDirection string // "desc" (default) or "asc"
	Limit         int    // default DefaultLimit, at most MaxLimit
	Offset        int
}

// DecodeQuery reads ?search=&searchField=&role=&banned=&sortBy=&sortDirection=&limit=&offset=.
func (l *ListUsers) DecodeQuery(q url.Values) error {
	f := Fields{}
	l.Search = strings.TrimSpace(q.Get("search"))
	l.SearchField = q.Get("searchField")
	l.Role = strings.TrimSpace(q.Get("role"))
	l.SortBy = q.Get("sortBy")
	l.SortDirection = q.Get("sortDirection")
	if v := q.Get("banned"); v != "" {
		b, err := strconv.ParseBool(v)
		f.Add(err != nil, "banned", "must be true or false")
		l.Banned = &b
	}
	l.Limit = queryInt(q, "limit", f)
	l.Offset = queryInt(q, "offset", f)
	return f.Err()
}

// Validate applies the defaults and checks every field.
func (l *ListUsers) Validate() error {
	f := Fields{}
	l.SearchField = orDefault(l.SearchField, "email")
	l.SortBy = orDefault(l.SortBy, "createdAt")
	l.SortDirection = orDefault(l.SortDirection, "desc")
	if l.Limit == 0 {
		l.Limit = DefaultLimit
	}
	f.Add(utf8.RuneCountInString(l.Search) > maxSearchLength, "search", "must be at most 255 characters")
	f.Add(l.SearchField != "email" && l.SearchField != "name", "searchField", "must be email or name")
	f.Add(l.Role != "" && !isRole(l.Role), "role", "must be a role name")
	f.Add(l.SortBy != "createdAt" && l.SortBy != "email" && l.SortBy != "name", "sortBy", "must be createdAt, email or name")
	f.Add(l.SortDirection != "asc" && l.SortDirection != "desc", "sortDirection", "must be asc or desc")
	f.Add(l.Limit < 1 || l.Limit > MaxLimit, "limit", "must be 1-100")
	f.Add(l.Offset < 0, "offset", "must be 0 or more")
	return f.Err()
}

// BanUser is the POST /users/{id}/ban body (Better Auth ban-user names).
type BanUser struct {
	BanReason string `json:"banReason"`
	// BanExpiresIn is the ban length in seconds; nil or 0 bans permanently.
	BanExpiresIn *int64 `json:"banExpiresIn"`
}

// A timed ban lasts from a minute to ten years; longer is permanent.
const (
	MinBanSeconds = 60
	MaxBanSeconds = 10 * 365 * 24 * 60 * 60
)

// Validate trims the reason and checks the expiry.
func (b *BanUser) Validate() error {
	f := Fields{}
	b.BanReason = strings.TrimSpace(b.BanReason)
	f.Add(b.BanReason == "" || utf8.RuneCountInString(b.BanReason) > maxBanReasonLength, "banReason", "must be 1-500 characters")
	if b.BanExpiresIn != nil {
		s := *b.BanExpiresIn
		f.Add(s < 0 || (s > 0 && s < MinBanSeconds) || s > MaxBanSeconds, "banExpiresIn", "must be 0 (permanent) or 60 seconds to 10 years")
	}
	return f.Err()
}

// SetUserRole is the PUT /users/{id}/role body (Better Auth set-role names).
// Which roles exist is a business rule (services.Roles).
type SetUserRole struct {
	Role string `json:"role"`
}

// Validate trims the role and checks its shape.
func (s *SetUserRole) Validate() error {
	s.Role = strings.TrimSpace(s.Role)
	f := Fields{}
	f.Add(!isRole(s.Role), "role", "must be a role name")
	return f.Err()
}

// isRole: lower-case letters, digits, "-" and "_", 1-32 characters.
func isRole(s string) bool {
	if s == "" || len(s) > maxRoleLength {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// queryInt parses q[key] as a base-10 int (0 when absent).
func queryInt(q url.Values, key string, f Fields) int {
	v := q.Get(key)
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	f.Add(err != nil, key, "must be a whole number")
	return n
}
