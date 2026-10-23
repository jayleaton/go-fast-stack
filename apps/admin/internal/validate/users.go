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
func (users *ListUsers) DecodeQuery(values url.Values) error {
	fields := Fields{}
	users.Search = strings.TrimSpace(values.Get("search"))
	users.SearchField = values.Get("searchField")
	users.Role = strings.TrimSpace(values.Get("role"))
	users.SortBy = values.Get("sortBy")
	users.SortDirection = values.Get("sortDirection")
	if value := values.Get("banned"); value != "" {
		flag, err := strconv.ParseBool(value)
		fields.Add(err != nil, "banned", "must be true or false")
		users.Banned = &flag
	}
	users.Limit = queryInt(values, "limit", fields)
	users.Offset = queryInt(values, "offset", fields)
	return fields.Err()
}

// Validate applies the defaults and checks every field.
func (users *ListUsers) Validate() error {
	fields := Fields{}
	users.SearchField = orDefault(users.SearchField, "email")
	users.SortBy = orDefault(users.SortBy, "createdAt")
	users.SortDirection = orDefault(users.SortDirection, "desc")
	if users.Limit == 0 {
		users.Limit = DefaultLimit
	}
	fields.Add(utf8.RuneCountInString(users.Search) > maxSearchLength, "search", "must be at most 255 characters")
	fields.Add(users.SearchField != "email" && users.SearchField != "name", "searchField", "must be email or name")
	fields.Add(users.Role != "" && !isRole(users.Role), "role", "must be a role name")
	fields.Add(users.SortBy != "createdAt" && users.SortBy != "email" && users.SortBy != "name", "sortBy", "must be createdAt, email or name")
	fields.Add(users.SortDirection != "asc" && users.SortDirection != "desc", "sortDirection", "must be asc or desc")
	fields.Add(users.Limit < 1 || users.Limit > MaxLimit, "limit", "must be 1-100")
	fields.Add(users.Offset < 0, "offset", "must be 0 or more")
	return fields.Err()
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
func (user *BanUser) Validate() error {
	fields := Fields{}
	user.BanReason = strings.TrimSpace(user.BanReason)
	fields.Add(user.BanReason == "" || utf8.RuneCountInString(user.BanReason) > maxBanReasonLength, "banReason", "must be 1-500 characters")
	if user.BanExpiresIn != nil {
		size := *user.BanExpiresIn
		fields.Add(size < 0 || (size > 0 && size < MinBanSeconds) || size > MaxBanSeconds, "banExpiresIn", "must be 0 (permanent) or 60 seconds to 10 years")
	}
	return fields.Err()
}

// SetUserRole is the PUT /users/{id}/role body (Better Auth set-role names).
// Which roles exist is a business rule (services.Roles).
type SetUserRole struct {
	Role string `json:"role"`
}

// Validate trims the role and checks its shape.
func (role *SetUserRole) Validate() error {
	role.Role = strings.TrimSpace(role.Role)
	fields := Fields{}
	fields.Add(!isRole(role.Role), "role", "must be a role name")
	return fields.Err()
}

// isRole: lower-case letters, digits, "-" and "_", 1-32 characters.
func isRole(text string) bool {
	if text == "" || len(text) > maxRoleLength {
		return false
	}
	for _, char := range text {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// queryInt parses q[key] as a base-10 int (0 when absent).
func queryInt(values url.Values, key string, fields Fields) int {
	value := values.Get(key)
	if value == "" {
		return 0
	}
	count, err := strconv.Atoi(value)
	fields.Add(err != nil, key, "must be a whole number")
	return count
}
