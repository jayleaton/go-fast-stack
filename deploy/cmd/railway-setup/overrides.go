package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// override is a platform.yaml key a person or agent can set without editing
// the file: --project or PLATFORM_PROJECT, and so on. Set values are written
// into platform.yaml (comments kept), so the file stays the record and a
// re-run without flags does the same thing.
type override struct {
	key, flagName, env, usage string
	value                     *string
}

func overrides() []*override {
	return []*override{
		{key: "project", flagName: "project", env: "PLATFORM_PROJECT", usage: "Railway project name (default from platform.yaml; auto: the repo name)"},
		{key: "workspace", flagName: "workspace", env: "PLATFORM_WORKSPACE", usage: "Railway workspace name or id (needed when you belong to several)"},
		{key: "environments", flagName: "environments", env: "PLATFORM_ENVIRONMENTS", usage: "production | staging,production | staging,qa,production"},
		{key: "admin", flagName: "admin", env: "PLATFORM_ADMIN", usage: "true|false: the admin dashboard and its own Postgres"},
		{key: "region", flagName: "region", env: "PLATFORM_REGION", usage: "Railway region for every service (\"\": Railway's default)"},
		{key: "repo", flagName: "repo", env: "PLATFORM_REPO", usage: "GitHub owner/name, auto (origin remote) or \"\" (upload only)"},
	}
}

// resolve picks each override from its flag, else its env var.
func (o *override) resolve(flagValue string, set bool) {
	switch {
	case set:
		o.value = &flagValue
	default:
		if v, ok := os.LookupEnv(o.env); ok {
			o.value = &v
		}
	}
}

// persist writes the set overrides into platform.yaml (validated first);
// it returns the keys it changed.
func persist(path string, ovs []*override) ([]string, error) {
	var keys []platform.Key
	for _, o := range ovs {
		if o.value == nil {
			continue
		}
		v := strings.TrimSpace(*o.value)
		if o.key == "admin" {
			if v = strings.ToLower(v); v != "true" && v != "false" {
				return nil, fmt.Errorf("--admin must be true or false, got %q", *o.value)
			}
		}
		keys = append(keys, platform.Key{Name: o.key, Value: v})
	}
	if len(keys) == 0 {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out, changed, err := platform.Edit(raw, keys)
	if err != nil || len(changed) == 0 {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	return changed, os.WriteFile(path, out, fi.Mode().Perm())
}
