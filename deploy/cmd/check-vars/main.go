// Command check-vars proves, offline, that `make railway-apply` cannot
// delete or overwrite a variable someone set by hand. It builds the IaC graph
// of every environment in platform.yaml exactly as `railway config plan`
// receives it (the project is `variables.managed`: Railway deletes every
// variable the graph neither sets nor ignores) and checks:
//
//   - every variable an app service reads (the names in the -env-example
//     files, narrowed per service by -reads) is either set by the graph or
//     matched by variables.ignore, so a value set in the dashboard survives
//     an apply;
//   - no variable the graph sets is also matched by variables.ignore, so the
//     two never disagree about who owns it;
//   - every -keep name is ignored (never set) on every service that reads
//     it: values a project sets by hand;
//   - a manual value in per_environment leaves its variable unset and
//     ignored;
//   - with secrets.provider doppler, no Doppler-owned secret
//     (platform.DopplerNames) stays a plain Railway variable by design: the
//     graph sets none but the database references, and no
//     unmanaged_variables pattern keeps one on Railway.
//
// It reads no network and no credentials. `make deploy-vars` runs it.
//
//	check-vars -config platform.yaml -env-example ../.env.example \
//	  -reads 'web=*,!ADMIN_*' -reads 'admin=ADMIN_*,DATABASE_URL' -keep STRIPE_SECRET_KEY
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jayleaton/go-fast-stack/deploy/platform"
)

// Exit codes.
const (
	exitOK       = 0
	exitProblems = 1 // apply could delete or overwrite a hand-set variable
	exitUsage    = 2 // bad flags or an unreadable file
)

// localOnly are .env.example names only a developer machine uses: make and
// docker compose, and ADMIN_PORT (on Railway admin listens on the PORT the
// graph sets, which its domain targets).
var localOnly = []string{"COMPOSE_PROJECT_NAME", "POSTGRES_PORT", "PGWEB_PORT", "TEST_POSTGRES_PORT", "ADMIN_PORT"}

var envLine = regexp.MustCompile(`^#?\s*([A-Z][A-Z0-9_]*)=`)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type list []string

func (values *list) String() string { return strings.Join(*values, ",") }
func (values *list) Set(value string) error {
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			*values = append(*values, item)
		}
	}
	return nil
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("check-vars", flag.ContinueOnError)
	flags.SetOutput(stderr)
	config := flags.String("config", "platform.yaml", "path to platform.yaml")
	var examples, keep, reads list
	flags.Var(&examples, "env-example", "env file listing the variables the apps read (repeatable)")
	flags.Var(&keep, "keep", "names that must never be set or deleted by apply (comma separated, repeatable)")
	flags.Var(&reads, "reads", "service=pattern,... : the names that service reads (glob; !pattern excludes); default every name (repeatable)")
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	cfg, err := platform.Load(*config)
	if err != nil {
		fmt.Fprintln(stderr, "check-vars:", err)
		return exitUsage
	}
	// Build needs a concrete project and repo; their values do not change
	// the variables.
	cfg.Project, cfg.Repo = "check-vars", "owner/repo"
	names, err := readNames(examples)
	if err != nil {
		fmt.Fprintln(stderr, "check-vars:", err)
		return exitUsage
	}

	var problems []string
	if cfg.Secrets.Provider == platform.ProviderDoppler {
		problems = append(problems, dopplerOwned(cfg)...)
	}
	for _, env := range cfg.Environments {
		graph, err := buildGraph(cfg, env)
		if err != nil {
			fmt.Fprintln(stderr, "check-vars:", err)
			return exitUsage
		}
		problems = append(problems, graph.check(cfg, env, names, keep, readers(reads))...)
		fmt.Fprintf(stdout, "%s: %s; ignored patterns %d; set by hand (manual): %v\n", env, graph.summary(), len(graph.Variables.Ignore), cfg.HandSet(env))
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			fmt.Fprintln(stderr, "check-vars:", problem)
		}
		fmt.Fprintf(stderr, "check-vars: %d problems: apply could delete or overwrite a hand-set variable (list it in platform.yaml unmanaged_variables), or a secret would stay a plain Railway variable\n", len(problems))
		return exitProblems
	}
	fmt.Fprintf(stdout, "check-vars: ok: %d app variables and %d kept names are set by IaC or ignored, never both, in every environment\n", len(names), len(keep))
	return exitOK
}

// readNames returns the variable names the env files list, set or commented.
func readNames(files []string) ([]string, error) {
	if len(files) == 0 {
		return nil, errors.New("pass at least one -env-example")
	}
	var out []string
	for _, name := range files {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			match := envLine.FindStringSubmatch(strings.TrimSpace(scanner.Text()))
			if match != nil && !slices.Contains(localOnly, match[1]) && !slices.Contains(out, match[1]) {
				out = append(out, match[1])
			}
		}
		_ = file.Close()
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	slices.Sort(out)
	return out, nil
}

// readers parses -reads into service -> patterns. Each flag value is
// "service=p1,p2"; the list flag splits on commas, so a bare pattern
// continues the service before it.
func readers(flags []string) map[string][]string {
	out := map[string][]string{}
	service := ""
	for _, value := range flags {
		if named, patterns, ok := strings.Cut(value, "="); ok {
			service, value = named, patterns
		}
		out[service] = append(out[service], value)
	}
	return out
}

// readsName reports whether a service with these patterns reads name: no
// patterns means every name; otherwise the last matching pattern decides
// (!pattern excludes).
func readsName(patterns []string, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	in := false
	for _, pattern := range patterns {
		exclude := strings.HasPrefix(pattern, "!")
		if ok, _ := path.Match(strings.TrimPrefix(pattern, "!"), name); ok {
			in = !exclude
		}
	}
	return in
}

// iacGraph is the part of the IaC graph that decides variables.
type iacGraph struct {
	Variables struct {
		Managed bool     `json:"managed"`
		Ignore  []string `json:"ignore"`
	} `json:"variables"`
	Resources []struct {
		Type      string                     `json:"type"`
		Name      string                     `json:"name"`
		Variables map[string]json.RawMessage `json:"variables"`
	} `json:"resources"`
}

// buildGraph builds env's graph and decodes it the way the CLI receives it.
func buildGraph(cfg platform.Config, env string) (iacGraph, error) {
	project, err := cfg.Build(env)
	if err != nil {
		return iacGraph{}, err
	}
	encoded, err := json.Marshal(project.Graph())
	if err != nil {
		return iacGraph{}, err
	}
	var graph iacGraph
	err = json.Unmarshal(encoded, &graph)
	return graph, err
}

// dopplerOwned reports unmanaged_variables patterns that would keep a
// Doppler-owned secret on Railway as a plain variable someone sets by hand.
func dopplerOwned(cfg platform.Config) []string {
	var out []string
	for _, service := range cfg.PublicServices() {
		for _, name := range platform.DopplerNames(service) {
			for _, pattern := range cfg.Unmanaged {
				if matchesPattern(pattern, service, name) {
					out = append(out, fmt.Sprintf("%s/%s is a secret Doppler serves, but unmanaged_variables %q keeps it as a plain Railway variable: narrow the pattern to non-secret names", service, name, pattern))
				}
			}
		}
	}
	return out
}

// matchesPattern reports whether a variables.ignore / unmanaged_variables
// pattern ("NAME", "NAME_*" or "service/NAME") matches name on service.
func matchesPattern(pattern, service, name string) bool {
	scope, glob, scoped := strings.Cut(pattern, "/")
	if !scoped {
		scope, glob = "", pattern
	}
	if scope != "" && scope != service {
		return false
	}
	ok, _ := path.Match(glob, name)
	return ok
}

func (graph iacGraph) ignored(service, name string) bool {
	return slices.ContainsFunc(graph.Variables.Ignore, func(pattern string) bool { return matchesPattern(pattern, service, name) })
}

func (graph iacGraph) summary() string {
	var parts []string
	for _, resource := range graph.Resources {
		if resource.Type == "service" {
			parts = append(parts, fmt.Sprintf("%s sets %d variables", resource.Name, len(resource.Variables)))
		}
	}
	return strings.Join(parts, ", ")
}

func (graph iacGraph) check(cfg platform.Config, env string, names, keep []string, readBy map[string][]string) []string {
	var out []string
	bad := func(format string, args ...any) { out = append(out, env+": "+fmt.Sprintf(format, args...)) }
	if !graph.Variables.Managed {
		bad("variables.managed is off: the checks below assume Railway deletes unlisted variables")
	}
	handSet := cfg.HandSet(env)
	for _, resource := range graph.Resources {
		if resource.Type != "service" {
			continue
		}
		service := resource.Name
		for name := range resource.Variables {
			if graph.ignored(service, name) {
				bad("%s/%s is set by the graph and also ignored: IaC would overwrite a hand-set value", service, name)
			}
			if slices.Contains(keep, name) && readsName(readBy[service], name) {
				bad("%s/%s must be kept by hand but the graph sets it", service, name)
			}
			if cfg.Secrets.Provider == platform.ProviderDoppler && slices.Contains(platform.DopplerNames(service), name) && !slices.Contains(platform.GraphSet(), name) {
				bad("%s/%s is a secret Doppler serves, but the graph sets it as a plain Railway variable", service, name)
			}
		}
		for _, name := range append(slices.Clone(names), keep...) {
			if !readsName(readBy[service], name) {
				continue
			}
			if _, set := resource.Variables[name]; !set && !graph.ignored(service, name) {
				bad("%s/%s is neither set by the graph nor ignored: apply deletes it if someone sets it by hand", service, name)
			}
		}
		for _, entry := range handSet {
			owner, name, _ := strings.Cut(entry, "/")
			if owner != service {
				continue
			}
			if _, set := resource.Variables[name]; set || !graph.ignored(service, name) {
				bad("%s/%s is manual but the graph would set or delete it", service, name)
			}
		}
	}
	return out
}
