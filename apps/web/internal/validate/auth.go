package validate

import "strings"

// The sign-in flows' inputs. They are shape only: packages/auth owns the
// rules (email syntax, password length, tokens) and reports them as its own
// errors, which the API passes on as named apiclient errors.

// SignUpEmail is an email and password sign-up.
type SignUpEmail struct {
	Name        string `json:"name"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	CallbackURL string `json:"callbackURL,omitempty"` // where the verification link lands
}

// Validate trims the name and email.
func (input *SignUpEmail) Validate() error {
	input.Name = strings.TrimSpace(input.Name)
	input.Email = strings.TrimSpace(input.Email)
	return nil
}

// SignInEmail is an email and password sign-in.
type SignInEmail struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	CallbackURL string `json:"callbackURL,omitempty"` // where a fresh verification link lands
}

// Validate trims the email.
func (input *SignInEmail) Validate() error {
	input.Email = strings.TrimSpace(input.Email)
	return nil
}

// CreatePasswordReset asks for a reset link that ends at RedirectTo?token=.
type CreatePasswordReset struct {
	Email      string `json:"email"`
	RedirectTo string `json:"redirectTo"`
}

// Validate trims the email.
func (input *CreatePasswordReset) Validate() error {
	input.Email = strings.TrimSpace(input.Email)
	return nil
}

// ResetPassword spends a reset link's token on a new password.
type ResetPassword struct {
	Token       string `json:"token"`
	NewPassword string `json:"newPassword"`
}

// Validate accepts any shape; auth checks the token and the password.
func (*ResetPassword) Validate() error { return nil }

// VerifyEmail spends an emailed verification token.
type VerifyEmail struct {
	Token string `json:"token"`
}

// Validate accepts any shape; auth checks the token.
func (*VerifyEmail) Validate() error { return nil }

// CreateVerificationEmail sends a fresh verification link.
type CreateVerificationEmail struct {
	Email       string `json:"email"`
	CallbackURL string `json:"callbackURL"`
}

// Validate trims the email.
func (input *CreateVerificationEmail) Validate() error {
	input.Email = strings.TrimSpace(input.Email)
	return nil
}
