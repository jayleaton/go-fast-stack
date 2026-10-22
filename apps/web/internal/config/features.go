package config

// Features are the environment-dependent toggles. Branch on these, never on Env.
type Features struct {
	// EmailVerification requires a verified email before sign-in.
	EmailVerification bool
	// TemplateWelcome shows the starter welcome pages. Set false, then delete
	// the template-welcome UI.
	TemplateWelcome bool
	// DevAuth signs every request in as a seeded dev user until packages/auth
	// is wired. Never on in production.
	DevAuth bool
}

// FeaturesFor returns the toggles for env.
func FeaturesFor(env Env) Features {
	if env == Production {
		return Features{EmailVerification: true, TemplateWelcome: true}
	}
	return Features{EmailVerification: false, TemplateWelcome: true, DevAuth: env == Development}
}
