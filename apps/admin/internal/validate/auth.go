package validate

import "strings"

// The admin sign-in flows' inputs. Shape only: packages/auth owns the rules
// (passwords, tokens, codes) and the API passes its errors on as named
// apiclient errors.

// Two-factor methods of VerifyTwoFactor.Method.
const (
	MethodTOTP       = "totp"
	MethodBackupCode = "backup_code"
)

// SignIn is an admin's email and password.
type SignIn struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// Validate trims the email.
func (input *SignIn) Validate() error {
	input.Email = strings.TrimSpace(input.Email)
	return nil
}

// SetPassword spends an invite or reset link's token on a password.
type SetPassword struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// Validate accepts any shape; auth checks the token and the password.
func (*SetPassword) Validate() error { return nil }

// CreatePasswordReset asks for a reset link.
type CreatePasswordReset struct {
	Email string `json:"email"`
}

// Validate trims the email.
func (input *CreatePasswordReset) Validate() error {
	input.Email = strings.TrimSpace(input.Email)
	return nil
}

// VerifyTwoFactor is the second sign-in step, against the challenge in the
// request's two-factor cookie.
type VerifyTwoFactor struct {
	Code        string `json:"code"`
	Method      string `json:"method"` // MethodTOTP or MethodBackupCode
	TrustDevice bool   `json:"trustDevice"`
}

// Validate trims the code.
func (input *VerifyTwoFactor) Validate() error {
	input.Code = strings.TrimSpace(input.Code)
	return nil
}

// EnableTwoFactor starts optional enrolment with the admin's password.
type EnableTwoFactor struct {
	Password string `json:"password"`
}

// Validate accepts any shape; auth checks the password.
func (*EnableTwoFactor) Validate() error { return nil }

// ConfirmTwoFactor is the first code from the authenticator app.
type ConfirmTwoFactor struct {
	Code string `json:"code"`
}

// Validate trims the code.
func (input *ConfirmTwoFactor) Validate() error {
	input.Code = strings.TrimSpace(input.Code)
	return nil
}
