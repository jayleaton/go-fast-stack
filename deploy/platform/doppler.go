package platform

import (
	"fmt"
	"slices"
)

// Doppler layout: one Doppler environment per Railway environment, plus dev
// for local work. The web service reads the environment's root config
// (prd, stg, ...); admin reads a branch of it (prd_admin, ...). A branch
// inherits the root's values (DATABASE_URL, RESEND_API_KEY), so they are
// set once, while admin-only secrets live in the branch, which a token for
// the root config cannot read: the web token can never see admin secrets.

// DopplerLocal is the Doppler environment (and config) for local development.
const DopplerLocal = "dev"

// DopplerEnvironment is the Doppler environment slug for a Railway
// environment: production prd, staging stg, qa qa.
func DopplerEnvironment(env string) string {
	switch env {
	case "production":
		return "prd"
	case "staging":
		return "stg"
	}
	return env
}

// DopplerConfig is the config one service reads in env.
func DopplerConfig(env, service string) string {
	base := DopplerEnvironment(env)
	if service == ServiceAdmin {
		return base + "_admin"
	}
	return base
}

// Source says where a seeded Doppler secret's first value comes from.
type Source int

const (
	IfOnRailway     Source = iota // the service's Railway value when it has one, else nothing (an optional secret)
	FromRailway                   // the service's current Railway value (it must exist)
	RailwayOrRandom               // the Railway value, else 48 random bytes
	RailwayOrResend               // the Railway value, else ResendPlaceholder
)

// Seed is a secret setup puts into a Doppler config when it is missing.
// Values already in Doppler are never overwritten: once seeded, Doppler is
// where a value changes.
type Seed struct {
	Name   string
	Source Source
}

// Doppler-owned names: what each service reads from Doppler once it is
// switched (SECRETS_PROVIDER=doppler). They must match the apps'
// liveSecrets (apps/*/internal/config/secrets.go): the manager reads only
// these from the provider, so a live secret missing here is empty after the
// switch. Everything else a service reads is a non-secret setting and stays
// a Railway variable. Auth secrets are copied from Railway when they exist:
// a new value would make the next boot refuse to start (the 2FA data keys
// are wrapped by the old one).
var (
	webDoppler = []Seed{
		{"DATABASE_URL", FromRailway},
		{"AUTH_SECRET", RailwayOrRandom},
		{"AUTH_SECRET_PREVIOUS", IfOnRailway},
		{"AUTH_SECRET_REVOKED", IfOnRailway},
		{"RESEND_API_KEY", RailwayOrResend},
	}
	// adminDoppler lives in the admin branch config, which the web token
	// cannot read.
	adminDoppler = []Seed{
		{"ADMIN_DATABASE_URL", FromRailway},
		{"ADMIN_AUTH_SECRET", RailwayOrRandom},
		{"ADMIN_AUTH_SECRET_PREVIOUS", IfOnRailway},
		{"ADMIN_AUTH_SECRET_REVOKED", IfOnRailway},
	}
	// adminShared are admin names the web service has too: set once in the
	// root config, which the admin branch inherits.
	adminShared = []string{"DATABASE_URL", "RESEND_API_KEY"}
)

// DopplerSeeds lists what a service's own config must hold, in seeding
// order (admin's shared names come from the root config, seeded by web).
func DopplerSeeds(service string) []Seed {
	if service == ServiceAdmin {
		return adminDoppler
	}
	return webDoppler
}

// DopplerNames are the Doppler-owned names a service reads.
func DopplerNames(service string) []string {
	var out []string
	for _, seed := range DopplerSeeds(service) {
		out = append(out, seed.Name)
	}
	if service == ServiceAdmin {
		out = append(out, adminShared...)
	}
	return out
}

// graphSet are Doppler-owned names the IaC graph sets as references to the
// Postgres services: they stay on Railway (ignored once the app reads
// Doppler) and are never removed as copies.
var graphSet = []string{"DATABASE_URL", "ADMIN_DATABASE_URL"}

// GraphSet lists the Doppler-owned names the graph sets on Railway.
func GraphSet() []string { return slices.Clone(graphSet) }

// RailwayCopies are a service's Railway variables made redundant once it
// reads Doppler (docs/secrets.md "Where secrets live"): every Doppler-owned name but
// the graph's database references.
func RailwayCopies(service string) []string {
	return slices.DeleteFunc(DopplerNames(service), func(name string) bool { return slices.Contains(graphSet, name) })
}

// DopplerTokenName names the service token setup creates, so a re-run (or a
// cleanup) can find it: "railway <project> <service>".
func DopplerTokenName(railwayProject, service string) string {
	return fmt.Sprintf("railway %s %s", railwayProject, service)
}
