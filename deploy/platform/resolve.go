package platform

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var githubRemote = regexp.MustCompile(`^(?:https?://(?:[^@/]+@)?github\.com/|ssh://git@github\.com/|git@github\.com:)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// Resolve fills project: auto and repo: auto from the checkout: repo is the
// GitHub owner/name of the origin remote ("" when origin is not on GitHub:
// upload mode), project is the repo name or else the checkout directory's.
// Pure, so it is tested; LoadResolved feeds it from git.
func (config Config) Resolve(originURL, checkoutDir string) (Config, error) {
	submatch := githubRemote.FindStringSubmatch(strings.TrimSpace(originURL))
	if config.Repo == Auto {
		config.Repo = ""
		if submatch != nil {
			config.Repo = submatch[1] + "/" + submatch[2]
		}
	}
	if config.Project == Auto {
		switch {
		case submatch != nil:
			config.Project = submatch[2]
		case checkoutDir != "":
			config.Project = filepath.Base(checkoutDir)
		default:
			return config, fmt.Errorf("project: auto needs a git checkout (or set project in platform.yaml)")
		}
		const maxProject = 32 // Railway's limit on project names
		if len(config.Project) > maxProject {
			config.Project = strings.TrimRight(config.Project[:maxProject], "-._ ")
		}
	}
	switch config.Secrets.Project {
	case "":
		config.Secrets.Project = DefaultDopplerProject
	case Auto:
		config.Secrets.Project = config.Project
	}
	return config, config.Validate()
}

// LoadResolved is Load plus Resolve against the git checkout holding path.
func LoadResolved(path string) (Config, error) {
	cfg, err := Load(path)
	if err != nil {
		return cfg, err
	}
	dir := filepath.Dir(path)
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output() //nolint:noctx // a local, instant git query
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return cfg.Resolve(git("remote", "get-url", "origin"), git("rev-parse", "--show-toplevel"))
}
