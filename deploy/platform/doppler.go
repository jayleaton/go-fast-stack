package platform

import "fmt"

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
	FromRailway     Source = iota // the service's current Railway value (it must exist)
	RailwayOrRandom               // the Railway value, else 48 random bytes
	RailwayOrResend               // the Railway value, else ResendPlaceholder
)

// Seed is a secret setup puts into a Doppler config when it is missing.
// Values already in Doppler are never overwritten.
type Seed struct {
	Name   string
	Source Source
}

// DopplerSeeds lists what a service's config must hold. Auth secrets are
// copied from Railway when they exist: a new value would make the next boot
// refuse to start (the 2FA data keys are wrapped by the old one).
func DopplerSeeds(service string) []Seed {
	if service == ServiceAdmin {
		return []Seed{ // DATABASE_URL and RESEND_API_KEY come from the root config
			{"ADMIN_DATABASE_URL", FromRailway},
			{"ADMIN_AUTH_SECRET", RailwayOrRandom},
		}
	}
	return []Seed{
		{"DATABASE_URL", FromRailway},
		{"AUTH_SECRET", RailwayOrRandom},
		{"RESEND_API_KEY", RailwayOrResend},
	}
}

// RailwayCopies are a service's Railway variables made redundant once it
// reads Doppler (README "Where secrets live"). DATABASE_URL stays: it is the
// IaC reference to the Postgres service and is ignored under Doppler.
func RailwayCopies(service string) []string {
	if service == ServiceAdmin {
		return []string{"ADMIN_AUTH_SECRET", "RESEND_API_KEY"}
	}
	return []string{"AUTH_SECRET", "RESEND_API_KEY"}
}

// DopplerTokenName names the service token setup creates, so a re-run (or a
// cleanup) can find it: "railway <project> <service>".
func DopplerTokenName(railwayProject, service string) string {
	return fmt.Sprintf("railway %s %s", railwayProject, service)
}
