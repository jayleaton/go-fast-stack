// Command vulncheck fails on a known vulnerability in any dependency of the
// repository (`make vulncheck`; `make check-fast` and `make ci` run it):
//
//   - every Go module (go.mod, deploy/go.mod, tools/go.mod,
//     tools/actionlint/go.mod) through govulncheck, pinned in tools/go.mod,
//     which reports at symbol (called), package (imported) and module
//     (required) level;
//   - every pnpm lockfile (apps/mobile-app) through `pnpm audit`.
//
// Any finding fails, reachable or not: a published fix means bump to it.
// The only way past a finding is an entry in scripts/vulncheck/allowlist.txt,
// for an advisory with no fix (or none its consumer can take) that this
// repository cannot reach; the reason says why. Go advisories are listed by
// GO- id, npm ones by GHSA id. Each entry carries a reason and a review date; an
// entry past its review date fails, an entry that no longer matches anything
// fails (delete it), and a Go advisory our code calls fails even when listed.
//
// Exit codes: 0 clean, 1 a finding or allowlist problem, 2 a scanner could
// not run (no network for the vulnerability database, pnpm missing). make
// check-fast warns on 2 instead of blocking the push.
//
//	go run ./scripts/vulncheck            # every module and lockfile
//	go run ./scripts/vulncheck -go-only   # skip pnpm audit
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	exitFindings   = 1
	exitScanFailed = 2
	allowlistPath  = "scripts/vulncheck/allowlist.txt"
	dateLayout     = time.DateOnly
	// scanTimeout bounds one scanner run (a cold vulnerability database
	// download plus a build of the module's package graph).
	scanTimeout = 10 * time.Minute
	// allowlistFields is an entry's fixed columns before the free-text reason.
	allowlistFields = 3
)

// goModule is a Go module directory and the package pattern that covers it:
// the tool-only modules have no packages of their own, only `tool`.
type goModule struct{ dir, pattern string }

var goModules = []goModule{
	{".", "./..."},
	{"deploy", "./..."},
	{"tools", "tool"},
	{"tools/actionlint", "tool"},
}

var pnpmDirs = []string{"apps/mobile-app"}

// finding is one advisory in one place (a Go module directory or a lockfile).
type finding struct {
	where    string // module directory or lockfile directory
	id       string // GO-… or GHSA-…
	aliases  []string
	pkg      string // the vulnerable module or npm package
	version  string
	fixed    string // empty: no fix published
	called   bool   // Go only: our code reaches a vulnerable symbol
	summary  string
	severity string
}

// entry is one allowlist line: `<id> <package> <review-by> <reason…>`.
type entry struct {
	id, pkg, reason string
	reviewBy        time.Time
	line            int
	used            bool
}

func main() {
	goOnly := flag.Bool("go-only", false, "scan only the Go modules (skip pnpm audit)")
	flag.Parse()
	os.Exit(run(*goOnly, time.Now()))
}

func run(goOnly bool, now time.Time) int {
	entries, err := readAllowlist(allowlistPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "vulncheck:", err)
		return exitFindings
	}
	var findings []finding
	complete := map[string]bool{ecosystemGo: true, ecosystemNpm: !goOnly}
	for _, module := range goModules {
		found, scanErr := scanGo(module)
		if scanErr != nil {
			fmt.Fprintf(os.Stderr, "vulncheck: govulncheck %s: %v\n", module.dir, scanErr)
			complete[ecosystemGo] = false
			continue
		}
		findings = append(findings, found...)
	}
	if !goOnly {
		for _, dir := range pnpmDirs {
			found, scanErr := scanPnpm(dir)
			if scanErr != nil {
				fmt.Fprintf(os.Stderr, "vulncheck: pnpm audit %s: %v\n", dir, scanErr)
				complete[ecosystemNpm] = false
				continue
			}
			findings = append(findings, found...)
		}
	}

	problems := judge(findings, entries, now, complete)
	scanFailed := !complete[ecosystemGo] || (!goOnly && !complete[ecosystemNpm])
	for _, problem := range problems {
		fmt.Println("FAIL ", problem)
	}
	switch {
	case len(problems) > 0:
		fmt.Printf("vulncheck: %d problem(s); bump to the fixed version, or see %s\n", len(problems), allowlistPath)
		return exitFindings
	case scanFailed:
		fmt.Println("vulncheck: a scanner could not run; nothing it covers was checked")
		return exitScanFailed
	}
	fmt.Printf("vulncheck: ok (%d Go modules, %d lockfiles, %d allowlisted)\n", len(goModules), len(pnpmDirs)*boolInt(!goOnly), countUsed(entries))
	return 0
}

// judge turns findings into failures. An entry that matches nothing is only
// reported when its scanner ran (complete: "go" for GO- ids, "npm" for the
// rest), since a skipped scan matches nothing.
func judge(findings []finding, entries []*entry, now time.Time, complete map[string]bool) []string {
	var problems []string
	for _, found := range findings {
		listed := match(entries, found)
		switch {
		case listed == nil:
			fix := "no fix published"
			if found.fixed != "" {
				fix = "fixed in " + found.fixed
			}
			reach := ""
			if found.called {
				reach = ", CALLED by our code"
			}
			severity := ""
			if found.severity != "" {
				severity = found.severity + ", "
			}
			problems = append(problems, fmt.Sprintf("%s: %s %s@%s (%s%s%s): %s", found.where, found.id, found.pkg, found.version, severity, fix, reach, found.summary))
		case found.called:
			problems = append(problems, fmt.Sprintf("%s: %s is allowlisted (line %d) as unreachable, but our code calls it: remove the usage", found.where, found.id, listed.line))
		}
	}
	for _, listed := range entries {
		if now.After(listed.reviewBy) {
			problems = append(problems, fmt.Sprintf("%s:%d: %s passed its review date %s: re-check it, then move the date or delete it", allowlistPath, listed.line, listed.id, listed.reviewBy.Format(dateLayout)))
		}
		if complete[ecosystem(listed.id)] && !listed.used {
			problems = append(problems, fmt.Sprintf("%s:%d: %s matches nothing any more: delete it", allowlistPath, listed.line, listed.id))
		}
	}
	sort.Strings(problems)
	return problems
}

const (
	ecosystemGo  = "go"
	ecosystemNpm = "npm"
)

func ecosystem(id string) string {
	if strings.HasPrefix(id, "GO-") {
		return ecosystemGo
	}
	return ecosystemNpm
}

func match(entries []*entry, found finding) *entry {
	for _, listed := range entries {
		if listed.pkg != found.pkg {
			continue
		}
		if listed.id == found.id || contains(found.aliases, listed.id) {
			listed.used = true
			return listed
		}
	}
	return nil
}

func readAllowlist(path string) ([]*entry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open allowlist: %w", err)
	}
	defer file.Close()
	var entries []*entry
	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) <= allowlistFields {
			return nil, fmt.Errorf("%s:%d: want `<id> <package> <review-by YYYY-MM-DD> <reason>`", path, lineNumber)
		}
		reviewBy, dateErr := time.Parse(dateLayout, fields[2])
		if dateErr != nil {
			return nil, fmt.Errorf("%s:%d: review date: %w", path, lineNumber, dateErr)
		}
		entries = append(entries, &entry{
			id: fields[0], pkg: fields[1], reviewBy: reviewBy, line: lineNumber,
			reason: strings.Join(fields[allowlistFields:], " "),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	return entries, nil
}

// govulncheck -format json: a stream of messages; only these fields matter.
type goMessage struct {
	OSV *struct {
		ID       string   `json:"id"`
		Aliases  []string `json:"aliases"`
		Summary  string   `json:"summary"`
		Database struct {
			URL string `json:"url"`
		} `json:"database_specific"`
	} `json:"osv"`
	Finding *struct {
		OSV          string `json:"osv"`
		FixedVersion string `json:"fixed_version"`
		Trace        []struct {
			Module   string `json:"module"`
			Version  string `json:"version"`
			Function string `json:"function"`
		} `json:"trace"`
	} `json:"finding"`
}

func scanGo(module goModule) ([]finding, error) {
	// The tools modfile pins govulncheck; it then scans module.dir's own go.mod.
	out, err := command("go", "tool", "-modfile=tools/go.mod", "govulncheck", "-C", module.dir, "-format", "json", module.pattern)
	if err != nil {
		return nil, err
	}
	type osvInfo struct {
		aliases []string
		summary string
	}
	osvs := map[string]osvInfo{}
	byID := map[string]*finding{}
	decoder := json.NewDecoder(bytes.NewReader(out))
	for {
		var message goMessage
		if decodeErr := decoder.Decode(&message); errors.Is(decodeErr, io.EOF) {
			break
		} else if decodeErr != nil {
			return nil, fmt.Errorf("decode govulncheck output: %w", decodeErr)
		}
		if message.OSV != nil {
			osvs[message.OSV.ID] = osvInfo{message.OSV.Aliases, message.OSV.Summary}
		}
		if message.Finding == nil || len(message.Finding.Trace) == 0 {
			continue
		}
		vulnerable := message.Finding.Trace[0]
		found := byID[message.Finding.OSV]
		if found == nil {
			found = &finding{
				where: module.dir, id: message.Finding.OSV, pkg: vulnerable.Module,
				version: vulnerable.Version, fixed: message.Finding.FixedVersion,
			}
			byID[found.id] = found
		}
		if vulnerable.Function != "" {
			found.called = true
		}
	}
	findings := make([]finding, 0, len(byID))
	for id, found := range byID {
		found.aliases = osvs[id].aliases
		found.summary = osvs[id].summary
		findings = append(findings, *found)
	}
	return findings, nil
}

// pnpm audit --json: advisories keyed by an internal number.
type pnpmReport struct {
	Advisories map[string]struct {
		GHSA     string   `json:"github_advisory_id"`
		CVEs     []string `json:"cves"`
		Module   string   `json:"module_name"`
		Severity string   `json:"severity"`
		Title    string   `json:"title"`
		Patched  string   `json:"patched_versions"`
		Findings []struct {
			Version string `json:"version"`
		} `json:"findings"`
	} `json:"advisories"`
}

// noPatch is how the npm advisory database says "no fixed version".
const noPatch = "<0.0.0"

func scanPnpm(dir string) ([]finding, error) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		return nil, fmt.Errorf("pnpm is not installed: %w", err)
	}
	// pnpm audit exits 1 when it finds anything; the JSON is what counts.
	out, err := command("pnpm", "--dir", dir, "audit", "--json")
	var report pnpmReport
	if jsonErr := json.Unmarshal(out, &report); jsonErr != nil {
		return nil, fmt.Errorf("unreadable pnpm audit output (%w): %w", err, jsonErr)
	}
	findings := make([]finding, 0, len(report.Advisories))
	for _, advisory := range report.Advisories {
		found := finding{
			where: filepath.Join(dir, "pnpm-lock.yaml"), id: advisory.GHSA, aliases: advisory.CVEs,
			pkg: advisory.Module, summary: advisory.Title, severity: advisory.Severity,
		}
		if advisory.Patched != noPatch {
			found.fixed = advisory.Patched
		}
		if len(advisory.Findings) > 0 {
			found.version = advisory.Findings[0].Version
		}
		findings = append(findings, found)
	}
	return findings, nil
}

func command(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), scanTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countUsed(entries []*entry) int {
	used := 0
	for _, listed := range entries {
		if listed.used {
			used++
		}
	}
	return used
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
