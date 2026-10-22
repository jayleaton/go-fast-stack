// Package model holds the plain data types repositories return. It has no
// database dependencies, so services can expose these types to transports.
package model

import (
	"errors"
	"time"
)

// ErrNotFound is returned by repositories when a row does not exist.
var ErrNotFound = errors.New("not found")

// User is the Better Auth "user" row without credentials.
type User struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"emailVerified"`
	Image         *string   `json:"image"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// UserProfileUpdate changes the non-credential user columns. Nil Name keeps the
// current name; SetImage with a nil Image clears it.
type UserProfileUpdate struct {
	Name     *string
	SetImage bool
	Image    *string
}

// UserDataSummary lists what the app stores for a user (shown before account
// deletion). Add a count here for every app table that references "user".
type UserDataSummary struct {
	User     User  `json:"user"`
	Sessions int64 `json:"sessions"`
}
