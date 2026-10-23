package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// check is one doctor result. Status: ok | warn | fail. Fix says what to do.
type check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func (setup *setup) report(check check) {
	setup.sum.Checks = append(setup.sum.Checks, check)
	mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[check.Status]
	setup.say("  %s %-16s %s", mark, check.Name, check.Detail)
	if check.Fix != "" && check.Status != "ok" {
		setup.say("    fix: %s", check.Fix)
	}
}

// doctor checks everything make railway-apply needs and prints fixes. Only
// failures of required checks make it exit non-zero (exitPreflight).
func (setup *setup) doctor(ctx context.Context) error {
	failed := false
	fail := func(check check) { check.Status = "fail"; failed = true; setup.report(check) }

	// Railway CLI and login (checkVersion already ran in main).
	who, err := setup.rw.output(ctx, nil, "whoami")
	if err != nil {
		fail(check{Name: "railway login", Detail: "not logged in", Fix: "railway login"})
		return withCode(exitPreflight, "preflight failed")
	}
	setup.report(check{Name: "railway cli", Status: "ok", Detail: strings.TrimSpace(firstLine(string(who)))})

	if ws, err := setup.workspaceID(ctx); err != nil {
		fail(check{Name: "workspace", Detail: err.Error(), Fix: "set workspace in deploy/platform.yaml or pass --workspace / PLATFORM_WORKSPACE"})
	} else {
		setup.report(check{Name: "workspace", Status: "ok", Detail: ws})
	}

	if _, err := exec.LookPath("go"); err != nil {
		fail(check{Name: "go", Detail: "go not on PATH (the Railway CLI evaluates deploy/.railway/railway.go with it)", Fix: "install Go (version in mise.toml): mise install"})
	} else {
		setup.report(check{Name: "go", Status: "ok", Detail: "found"})
	}

	if check := setup.checkSSH(ctx); check.Status == "fail" {
		fail(check)
	} else {
		setup.report(check)
	}

	// Project, source and secrets: what apply will do.
	if err := setup.findProject(ctx); err != nil {
		fail(check{Name: "project", Detail: err.Error()})
	} else if setup.project == nil {
		setup.report(check{Name: "project", Status: "ok", Detail: "does not exist yet: apply creates " + setup.cfg.Project})
	} else {
		setup.report(check{Name: "project", Status: "ok", Detail: setup.project.Name + " (" + setup.project.ID + ")"})
	}
	setup.report(setup.checkRepo(ctx))
	if setup.cfg.Secrets.Provider == platform.ProviderDoppler {
		setup.report(setup.checkDopplerCLI(ctx))
		setup.report(setup.checkDoppler(ctx))
	}
	if failed {
		return withCode(exitPreflight, "preflight failed: fix the ✗ items above")
	}
	return nil
}

func firstLine(text string) string { line, _, _ := strings.Cut(text, "\n"); return line }

// checkSSH: the second-environment database password fix runs psql over
// railway ssh, which needs a local key registered with Railway.
func (setup *setup) checkSSH(ctx context.Context) check {
	result := check{Name: "ssh key", Fix: "railway ssh keys add (registers ~/.ssh/id_ed25519.pub); needed when an environment after the first gets a database"}
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
	if err := setup.rw.gql(ctx, `query { sshPublicKeys { edges { node { fingerprint name } } } }`, nil, &res); err != nil {
		result.Status, result.Detail = "warn", "could not list Railway SSH keys: "+err.Error()
		return result
	}
	registered := map[string]string{}
	for _, edge := range res.Keys.Edges {
		registered[edge.Node.Fingerprint] = edge.Node.Name
	}
	for _, fp := range localKeyFingerprints(ctx) {
		if name, ok := registered[fp]; ok {
			result.Status, result.Detail = "ok", "registered as "+name
			return result
		}
	}
	result.Status = "warn"
	result.Detail = "no local key is registered with Railway"
	if len(setup.cfg.Environments) > 1 {
		result.Status = "fail" // the fix is required for every environment after the first
		result.Detail += " (needed: more than one environment)"
	}
	return result
}

// localKeyFingerprints lists SHA256 fingerprints of ~/.ssh/*.pub and the
// agent's keys.
func localKeyFingerprints(ctx context.Context) []string {
	var out []string
	home, _ := os.UserHomeDir()
	pubs, _ := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	for _, pub := range pubs {
		output, err := exec.CommandContext(ctx, "ssh-keygen", "-lf", pub).Output()
		if err == nil {
			out = append(out, fingerprints(string(output))...)
		}
	}
	if output, err := exec.CommandContext(ctx, "ssh-add", "-l").Output(); err == nil {
		out = append(out, fingerprints(string(output))...)
	}
	return out
}

func fingerprints(listing string) []string {
	var out []string
	for line := range strings.SplitSeq(listing, "\n") {
		for key := range strings.FieldsSeq(line) {
			if strings.HasPrefix(key, "SHA256:") {
				out = append(out, key)
			}
		}
	}
	return out
}

func (setup *setup) checkRepo(ctx context.Context) check {
	result := check{Name: "github source"}
	switch {
	case setup.cfg.Upload():
		result.Status, result.Detail = "ok", `repo: "" (deploys by upload: make railway-up)`
	case setup.repoConnected(ctx):
		result.Status, result.Detail = "ok", setup.cfg.Repo+" connected: deploys on push"
	default:
		known, has := setup.repoAccess(ctx)
		switch {
		case known && has:
			result.Status, result.Detail = "ok", "Railway can read "+setup.cfg.Repo
		case known:
			result.Status, result.Detail = "warn", "Railway cannot read "+setup.cfg.Repo+": apply deploys by upload until it can"
			result.Fix = "install the Railway GitHub app: " + installURL(ctx, setup.cfg.Repo) + ", then re-run make railway-apply"
		default:
			result.Status, result.Detail = "ok", setup.cfg.Repo+": apply tries to connect it and deploys by upload if Railway cannot read it yet"
		}
	}
	return result
}

func (setup *setup) checkDoppler(ctx context.Context) check {
	result := check{Name: "doppler tokens", Status: "ok"}
	if setup.project == nil {
		result.Detail = "optional; services run on SECRETS_PROVIDER=env until setup moves them to Doppler (doppler login, then make railway-setup or make doppler-setup)"
		return result
	}
	var missing, staged, copies []string
	for _, env := range setup.cfg.Environments {
		for _, svc := range setup.services() {
			if setup.project.Environments[env] == "" || setup.project.Services[svc] == "" {
				continue
			}
			vars, err := setup.variables(ctx, svc, env)
			switch {
			case err != nil:
			case setup.onDoppler(vars):
				if left := copiesOn(vars, svc); len(left) > 0 {
					copies = append(copies, env+"/"+svc+" ("+strings.Join(left, ", ")+")")
				}
			case setup.cfg.DopplerMode(env) == platform.SecretsStaged && realToken(vars):
				staged = append(staged, env+"/"+svc)
			default:
				missing = append(missing, env+"/"+svc)
			}
		}
	}
	switch {
	case len(copies) > 0:
		result.Status = "warn"
		result.Detail = "secrets still set as Railway variables on services that read Doppler: " + strings.Join(copies, "; ")
		result.Fix = "make doppler-setup (removes them once the service is healthy)"
	case len(missing) > 0:
		result.Status = "warn"
		result.Detail = "optional; still on SECRETS_PROVIDER=env: " + strings.Join(missing, ", ")
		result.Fix = "doppler login, then make doppler-setup"
	default:
		result.Detail = "every service reads Doppler"
	}
	if len(staged) > 0 {
		result.Detail += "; staged (token sealed, still on its Railway secrets until per_environment.<env>.secrets is doppler): " + strings.Join(staged, ", ")
	}
	return result
}

// checkDopplerCLI: the CLI (pinned in mise.toml), its login and the project.
// Optional: without it setup keeps secrets in Railway (SECRETS_PROVIDER=env).
func (setup *setup) checkDopplerCLI(ctx context.Context) check {
	result := check{Name: "doppler cli", Status: "warn"}
	cli, err := newDoppler(setup.cfg.Secrets.Project)
	if err != nil {
		result.Detail, result.Fix = "not installed (optional: secrets stay in Railway until you add it)", "mise install, then doppler login"
		return result
	}
	version := cli.version(ctx)
	ws, err := cli.whoami(ctx)
	if err != nil {
		result.Detail, result.Fix = "doppler "+version+": not logged in", "doppler login"
		return result
	}
	if version != dopplerVersion {
		result.Detail, result.Fix = "doppler "+version+" (mise.toml pins "+dopplerVersion+"), workplace "+ws, "mise install"
		return result
	}
	projects, err := cli.names(ctx, "projects", "name")
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	result.Status, result.Detail = "ok", "doppler "+version+", workplace "+ws+", project "+setup.cfg.Secrets.Project
	if !projects[setup.cfg.Secrets.Project] {
		result.Detail += " (setup creates it)"
	}
	return result
}
