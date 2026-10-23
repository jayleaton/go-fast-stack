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
func (invite *CreateAdminInvite) Validate() error {
	invite.Email = strings.ToLower(strings.TrimSpace(invite.Email))
	invite.Name = strings.TrimSpace(invite.Name)
	if invite.Name == "" {
		invite.Name, _, _ = strings.Cut(invite.Email, "@")
	}
	fields := Fields{}
	fields.Add(!isEmail(invite.Email), "email", "must be a valid email address")
	fields.Add(utf8.RuneCountInString(invite.Name) > maxNameLength, "name", "must be at most 255 characters")
	return fields.Err()
}

// ResetAdminTwoFactor names the admin whose 2FA `admin reset-2fa <email>`
// removes. The exact address is required: there is no "all" or wildcard.
type ResetAdminTwoFactor struct {
	Email string
}

// Validate normalises the address.
func (reset *ResetAdminTwoFactor) Validate() error {
	reset.Email = strings.ToLower(strings.TrimSpace(reset.Email))
	fields := Fields{}
	fields.Add(!isEmail(reset.Email), "email", "must be a valid email address")
	return fields.Err()
}

// ListAuditLog is the query of GET /audit, newest first.
type ListAuditLog struct {
	Action   string // filter, e.g. "user.ban"; optional
	TargetID string // filter: entries about one user or admin; optional
	Limit    int    // default DefaultLimit, at most MaxLimit
	Offset   int
}

// DecodeQuery reads ?action=&targetId=&limit=&offset=.
func (log *ListAuditLog) DecodeQuery(values url.Values) error {
	fields := Fields{}
	log.Action = strings.TrimSpace(values.Get("action"))
	log.TargetID = strings.TrimSpace(values.Get("targetId"))
	log.Limit = queryInt(values, "limit", fields)
	log.Offset = queryInt(values, "offset", fields)
	return fields.Err()
}

// Validate applies the default page size and checks every field.
func (log *ListAuditLog) Validate() error {
	if log.Limit == 0 {
		log.Limit = DefaultLimit
	}
	fields := Fields{}
	fields.Add(len(log.Action) > maxAuditActionLength, "action", "must be at most 64 characters")
	fields.Add(len(log.TargetID) > maxAuditTargetLength, "targetId", "must be at most 255 characters")
	fields.Add(log.Limit < 1 || log.Limit > MaxLimit, "limit", "must be 1-100")
	fields.Add(log.Offset < 0, "offset", "must be 0 or more")
	return fields.Err()
}

func isEmail(text string) bool {
	address, err := mail.ParseAddress(text)
	return err == nil && address.Address == text && len(text) <= maxEmailLength
}
