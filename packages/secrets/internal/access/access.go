// Package access is the private channel between package secrets and package
// reveal. Go's internal rule keeps everything outside packages/secrets/...
// from importing it, so reveal.Value is the only way to read a Secret's value.
package access

// Reveal returns the value of a secrets.Secret, and false for anything else.
// Package secrets sets it in an init func.
var Reveal func(any) (string, bool)
