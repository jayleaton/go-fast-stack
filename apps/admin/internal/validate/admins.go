package validate

import (
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"
)

// CreateAdminInvite is the POST /admins/invites body: who to invite. The
// invitee sets their own password from the emailed link.
type CreateAdminInvite struct {
	Email string `json:"email"`
	// Name defaults to the part of Email before the @.
	Name string `json:"name"`
}

// Validate normalises the address and name.
func (c *CreateAdminInvite) Validate() error {
	c.Email = strings.ToLower(strings.TrimSpace(c.Email))
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name, _, _ = strings.Cut(c.Email, "@")
	}
	f := Fields{}
	f.Add(!isEmail(c.Email), "email", "must be a valid email address")
	f.Add(utf8.RuneCountInString(c.Name) > 255, "name", "must be at most 255 characters")
	return f.Err()
}

// ListAuditLog is the query of GET /audit, newest first.
type ListAuditLog struct {
	Action   string // filter, e.g. "user.ban"; optional
	TargetID string // filter: entries about one user or admin; optional
	Limit    int    // default DefaultLimit, at most MaxLimit
	Offset   int
}

// DecodeQuery reads ?action=&targetId=&limit=&offset=.
func (l *ListAuditLog) DecodeQuery(q url.Values) error {
	f := Fields{}
	l.Action = strings.TrimSpace(q.Get("action"))
	l.TargetID = strings.TrimSpace(q.Get("targetId"))
	l.Limit = queryInt(q, "limit", f)
	l.Offset = queryInt(q, "offset", f)
	return f.Err()
}

// Validate applies the default page size and checks every field.
func (l *ListAuditLog) Validate() error {
	if l.Limit == 0 {
		l.Limit = DefaultLimit
	}
	f := Fields{}
	f.Add(len(l.Action) > 64, "action", "must be at most 64 characters")
	f.Add(len(l.TargetID) > 255, "targetId", "must be at most 255 characters")
	f.Add(l.Limit < 1 || l.Limit > MaxLimit, "limit", "must be 1-100")
	f.Add(l.Offset < 0, "offset", "must be 0 or more")
	return f.Err()
}

func isEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && len(s) <= 255
}
