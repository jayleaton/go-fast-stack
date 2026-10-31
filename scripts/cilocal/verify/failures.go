package verify

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// What a failed step printed that names tests or files, and the one line of
// triage the run prints and the PR shows: whether this branch changed those
// files. The rule it states (AGENTS.md "Verifying"): a failure in what this
// branch changed is fixed in this branch; any other is fixed forward in its
// own pull request off main, with a regression test, and the head is never
// re-run until green.

var (
	ansi       = regexp.MustCompile("\x1b\\[[0-9;]*m")
	goTestFail = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	goPkgFail  = regexp.MustCompile(`^FAIL\s+([\w.-]+(?:/[\w.-]+)+)\s`)
	// golangci-lint, the compiler and vet: "path/file.go:12:3: message".
	fileLine = regexp.MustCompile(`^(?:\./)?([\w./-]+\.(?:go|templ|sql|sh|ya?ml|ts|tsx)):\d+(?::\d+)?:? `)
)

// Failures collects a failed step's output lines and reads what failed.
type Failures struct {
	module  string
	pending []string // failed tests whose package line has not come yet
	named   []string // "<package> <test>"
	pkgs    []string
	files   []string
}

// NewFailures reads output of a root module whose path is `module`.
func NewFailures(module string) *Failures {
	return &Failures{module: module, pending: nil, named: nil, pkgs: nil, files: nil}
}

func appendNew(list []string, value string) []string {
	if slices.Contains(list, value) {
		return list
	}
	return append(list, value)
}

// Line takes one line of the step's output.
func (failures *Failures) Line(line string) {
	line = ansi.ReplaceAllString(line, "")
	// go test prints a package's failed tests, then its FAIL line.
	if match := goTestFail.FindStringSubmatch(line); match != nil {
		failures.pending = appendNew(failures.pending, match[1])
	} else if match := goPkgFail.FindStringSubmatch(line); match != nil {
		failures.pkgs = appendNew(failures.pkgs, match[1])
		for _, test := range failures.pending {
			failures.named = appendNew(failures.named, match[1]+" "+test)
		}
		failures.pending = nil
	} else if match := fileLine.FindStringSubmatch(line); match != nil {
		failures.files = appendNew(failures.files, match[1])
	}
}

// Failure is what failed and where the fix goes. `changed` is the branch's
// changed files.
func (failures *Failures) Failure(changed []string) *Failure {
	named := append(append([]string{}, failures.named...), failures.pending...)
	if len(named) == 0 {
		named = append(named, failures.files...)
	}
	return &Failure{Tests: named, Hint: failures.hint(changed)}
}

func (failures *Failures) hint(changed []string) string {
	dirs := []string{}
	for _, pkg := range failures.pkgs {
		dirs = append(dirs, strings.TrimPrefix(strings.TrimPrefix(pkg, failures.module), "/"))
	}
	for _, file := range failures.files {
		dirs = append(dirs, path.Dir(file))
	}
	if len(dirs) == 0 {
		return "read the step's log: nothing in it names a test or file"
	}
	touched := []string{}
	for _, dir := range dirs {
		for _, file := range changed {
			if path.Dir(file) == dir || slices.Contains(failures.files, file) {
				touched = appendNew(touched, file)
			}
		}
	}
	if len(touched) > 0 {
		return fmt.Sprintf("this branch changed %s: fix it in this PR", strings.Join(touched, ", "))
	}
	return "this branch changed none of those files: fix it forward in its own PR off main with a regression test, then rebase this one on it"
}
