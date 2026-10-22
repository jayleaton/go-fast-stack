package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jayleaton/ai-patterns-go/deploy/platform"
)

// check is one doctor result. Status: ok | warn | fail. Fix says what to do.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func (s *setup) report(c check) {
	s.sum.Checks = append(s.sum.Checks, c)
	mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
	s.say("  %s %-16s %s", mark, c.Name, c.Detail)
	if c.Fix != "" && c.Status != "ok" {
		s.say("    fix: %s", c.Fix)
	}
}

// doctor checks everything make railway-apply needs and prints fixes. Only
// failures of required checks make it exit non-zero (exitPreflight).
func (s *setup) doctor(ctx context.Context) error {
	failed := false
	fail := func(c check) { c.Status = "fail"; failed = true; s.report(c) }

	// Railway CLI and login (checkVersion already ran in main).
	who, err := s.rw.output(ctx, nil, "whoami")
	if err != nil {
		fail(check{Name: "railway login", Detail: "not logged in", Fix: "railway login"})
		return withCode(exitPreflight, "preflight failed")
	}
	s.report(check{Name: "railway cli", Status: "ok", Detail: strings.TrimSpace(firstLine(string(who)))})

	if ws, err := s.workspaceID(ctx); err != nil {
		fail(check{Name: "workspace", Detail: err.Error(), Fix: "set workspace in deploy/platform.yaml or pass --workspace / PLATFORM_WORKSPACE"})
	} else {
		s.report(check{Name: "workspace", Status: "ok", Detail: ws})
	}

	if _, err := exec.LookPath("go"); err != nil {
		fail(check{Name: "go", Detail: "go not on PATH (the Railway CLI evaluates deploy/.railway/railway.go with it)", Fix: "install Go (version in mise.toml): mise install"})
	} else {
		s.report(check{Name: "go", Status: "ok", Detail: "found"})
	}

	if c := s.checkSSH(ctx); c.Status == "fail" {
		fail(c)
	} else {
		s.report(c)
	}

	// Project, source and secrets: what apply will do.
	if err := s.findProject(ctx); err != nil {
		fail(check{Name: "project", Detail: err.Error()})
	} else if s.project == nil {
		s.report(check{Name: "project", Status: "ok", Detail: "does not exist yet: apply creates " + s.cfg.Project})
	} else {
		s.report(check{Name: "project", Status: "ok", Detail: s.project.Name + " (" + s.project.ID + ")"})
	}
	s.report(s.checkRepo(ctx))
	if s.cfg.Secrets.Provider == platform.ProviderDoppler {
		s.report(s.checkDoppler(ctx))
	}
	if failed {
		return withCode(exitPreflight, "preflight failed: fix the ✗ items above")
	}
	return nil
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// checkSSH: the second-environment database password fix runs psql over
// railway ssh, which needs a local key registered with Railway.
func (s *setup) checkSSH(ctx context.Context) check {
	c := check{Name: "ssh key", Fix: "railway ssh keys add (registers ~/.ssh/id_ed25519.pub); needed when an environment after the first gets a database"}
	var res struct {
		Keys struct {
			Edges []struct {
				Node struct {
					Fingerprint string `json:"fingerprint"`
					Name        string `json:"name"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"sshPublicKeys"`
	}
	if err := s.rw.gql(ctx, `query { sshPublicKeys { edges { node { fingerprint name } } } }`, nil, &res); err != nil {
		c.Status, c.Detail = "warn", "could not list Railway SSH keys: "+err.Error()
		return c
	}
	registered := map[string]string{}
	for _, e := range res.Keys.Edges {
		registered[e.Node.Fingerprint] = e.Node.Name
	}
	for _, fp := range localKeyFingerprints(ctx) {
		if name, ok := registered[fp]; ok {
			c.Status, c.Detail = "ok", "registered as "+name
			return c
		}
	}
	c.Status = "warn"
	c.Detail = "no local key is registered with Railway"
	if len(s.cfg.Environments) > 1 {
		c.Status = "fail" // the fix is required for every environment after the first
		c.Detail += " (needed: more than one environment)"
	}
	return c
}

// localKeyFingerprints lists SHA256 fingerprints of ~/.ssh/*.pub and the
// agent's keys.
func localKeyFingerprints(ctx context.Context) []string {
	var out []string
	home, _ := os.UserHomeDir()
	pubs, _ := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	for _, p := range pubs {
		b, err := exec.CommandContext(ctx, "ssh-keygen", "-lf", p).Output()
		if err == nil {
			out = append(out, fingerprints(string(b))...)
		}
	}
	if b, err := exec.CommandContext(ctx, "ssh-add", "-l").Output(); err == nil {
		out = append(out, fingerprints(string(b))...)
	}
	return out
}

func fingerprints(listing string) []string {
	var out []string
	for line := range strings.SplitSeq(listing, "\n") {
		for f := range strings.FieldsSeq(line) {
			if strings.HasPrefix(f, "SHA256:") {
				out = append(out, f)
			}
		}
	}
	return out
}

func (s *setup) checkRepo(ctx context.Context) check {
	c := check{Name: "github source"}
	switch {
	case s.cfg.Upload():
		c.Status, c.Detail = "ok", `repo: "" (deploys by upload: make railway-up)`
	case s.repoConnected(ctx):
		c.Status, c.Detail = "ok", s.cfg.Repo+" connected: deploys on push"
	default:
		known, has := s.repoAccess(ctx)
		switch {
		case known && has:
			c.Status, c.Detail = "ok", "Railway can read "+s.cfg.Repo
		case known:
			c.Status, c.Detail = "warn", "Railway cannot read "+s.cfg.Repo+": apply deploys by upload until it can"
			c.Fix = "install the Railway GitHub app: " + installURL(ctx, s.cfg.Repo) + ", then re-run make railway-apply"
		default:
			c.Status, c.Detail = "ok", s.cfg.Repo+": apply tries to connect it and deploys by upload if Railway cannot read it yet"
		}
	}
	return c
}

func (s *setup) checkDoppler(ctx context.Context) check {
	c := check{Name: "doppler tokens", Status: "ok"}
	if s.project == nil {
		c.Detail = "optional; services start on SECRETS_PROVIDER=env until you add tokens (make railway-secrets)"
		return c
	}
	var missing []string
	for _, env := range s.cfg.Environments {
		for _, svc := range s.services() {
			if s.project.Environments[env] == "" || s.project.Services[svc] == "" {
				continue
			}
			vars, err := s.variables(ctx, svc, env)
			if err == nil && !s.usesDoppler(vars) {
				missing = append(missing, env+"/"+svc)
			}
		}
	}
	if len(missing) > 0 {
		c.Status = "warn"
		c.Detail = "optional; still on SECRETS_PROVIDER=env: " + strings.Join(missing, ", ")
		c.Fix = "make railway-secrets (one read-only Doppler service token per service and environment)"
		return c
	}
	c.Detail = "every service reads Doppler"
	return c
}
