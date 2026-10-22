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
func (c Config) Resolve(originURL, checkoutDir string) (Config, error) {
	m := githubRemote.FindStringSubmatch(strings.TrimSpace(originURL))
	if c.Repo == Auto {
		c.Repo = ""
		if m != nil {
			c.Repo = m[1] + "/" + m[2]
		}
	}
	if c.Project == Auto {
		switch {
		case m != nil:
			c.Project = m[2]
		case checkoutDir != "":
			c.Project = filepath.Base(checkoutDir)
		default:
			return c, fmt.Errorf("project: auto needs a git checkout (or set project in platform.yaml)")
		}
		if len(c.Project) > 32 { // Railway's limit
			c.Project = strings.TrimRight(c.Project[:32], "-._ ")
		}
	}
	if c.Secrets.Project == Auto || c.Secrets.Project == "" {
		c.Secrets.Project = c.Project
	}
	return c, c.Validate()
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
