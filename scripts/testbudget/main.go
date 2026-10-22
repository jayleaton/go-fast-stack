// Command testbudget keeps the test suite fast as it grows. It reads
// `go test -json` on stdin, prints what plain `go test` would (ok/FAIL lines,
// the output of failed tests, build errors), lists the slowest tests, and
// fails when a package's test binary or a single test runs longer than its
// budget. A single test is a leaf: a test without subtests, or one subtest
// (a parent's time includes its parallel subtests; the package budget
// bounds their sum). `make test` and CI pipe every run through it.
//
//	go test -race -json ./... | go run ./scripts/testbudget
//
// Budgets are wall time on a 2-vCPU CI runner under -race, test execution
// only (compiling is not counted). A test over budget is a design problem,
// not a flake: see AGENTS.md "Fast tests" before raising a budget.
//
// The table subcommand renders saved -json runs as markdown for
// `make bench-devloop`:
//
//	testbudget table race=race.json norace=norace.json
package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// Default budgets. Raise one only with a reason in the commit message.
const (
	defaultPackageBudget = 30 * time.Second
	defaultTestBudget    = 5 * time.Second
)

// event is one line of `go test -json` (cmd/test2json).
type event struct {
	Action     string
	Package    string
	Test       string
	Elapsed    float64
	Output     string
	ImportPath string // build-output, build-fail
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "table" {
		os.Exit(table(os.Stdout, os.Args[2:]))
	}
	pkgBudget := flag.Duration("pkg", defaultPackageBudget, "budget for one package's test binary (0: off)")
	testBudget := flag.Duration("test", defaultTestBudget, "budget for one leaf test or subtest (0: off)")
	top := flag.Int("top", 10, "list this many slowest tests at the end")
	flag.Parse()
	os.Exit(run(os.Stdin, os.Stdout, *pkgBudget, *testBudget, *top))
}

type result struct {
	name    string // package, or "package Test"
	elapsed time.Duration
}

// run streams a go test -json run and returns the exit code.
func run(in io.Reader, out io.Writer, pkgBudget, testBudget time.Duration, top int) int {
	w := bufio.NewWriter(out)
	defer func() { _ = w.Flush() }()

	type key struct{ pkg, test string }
	logs := map[key][]string{}
	var pkgs, tests []result
	failed := false

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		line := sc.Bytes()
		var e event
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &e) != nil {
			fmt.Fprintf(w, "%s\n", line) // not test2json: pass through
			continue
		}
		k := key{e.Package, e.Test}
		switch e.Action {
		case "build-output":
			fmt.Fprint(w, e.Output)
		case "build-fail":
			failed = true
		case "output":
			if !strings.HasPrefix(e.Output, "=== ") {
				logs[k] = append(logs[k], e.Output)
			}
		case "pass", "skip", "fail":
			lines := logs[k]
			delete(logs, k)
			d := seconds(e.Elapsed)
			if e.Action == "fail" {
				failed = true
				for _, l := range lines {
					fmt.Fprint(w, l)
				}
				break
			}
			if e.Test != "" {
				if e.Action == "pass" {
					tests = append(tests, result{e.Package + " " + e.Test, d})
				}
				break
			}
			// A passing package prints only its summary, as plain go test does.
			for _, l := range lines {
				if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "?") || strings.HasPrefix(l, "\t") {
					fmt.Fprint(w, l)
				}
			}
			if e.Action == "pass" {
				pkgs = append(pkgs, result{e.Package, d})
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(w, "testbudget: read:", err)
		return 1
	}

	byTime := func(a, b result) int { return cmp.Compare(b.elapsed, a.elapsed) }
	tests = leaves(tests)
	slices.SortFunc(tests, byTime)
	slices.SortFunc(pkgs, byTime)
	if top > 0 && len(tests) > 0 {
		fmt.Fprintf(w, "\nslowest tests:\n")
		for _, t := range tests[:min(top, len(tests))] {
			fmt.Fprintf(w, "  %7.2fs  %s\n", t.elapsed.Seconds(), t.name)
		}
	}

	over := overBudget(pkgs, pkgBudget, "package")
	over = append(over, overBudget(tests, testBudget, "test")...)
	if len(over) > 0 {
		fmt.Fprintf(w, "\nFAIL  over the test-time budget:\n")
		for _, o := range over {
			fmt.Fprintf(w, "  %s\n", o)
		}
		fmt.Fprintf(w, "make it fast (AGENTS.md \"Fast tests\"): t.Parallel(), dbtest clones, fake clocks, no sleeps\n")
		failed = true
	}
	if failed {
		return 1
	}
	return 0
}

func overBudget(rs []result, budget time.Duration, kind string) []string {
	var over []string
	for _, r := range rs {
		if budget > 0 && r.elapsed > budget {
			over = append(over, fmt.Sprintf("%s %s: %.2fs (budget %s)", kind, r.name, r.elapsed.Seconds(), budget))
		}
	}
	return over
}

// leaves drops tests that have subtests ("pkg Test" when "pkg Test/x" ran).
func leaves(rs []result) []result {
	parents := map[string]bool{}
	for _, r := range rs {
		for i := len(r.name) - 1; i > 0; i-- {
			if r.name[i] == '/' {
				parents[r.name[:i]] = true
			}
		}
	}
	return slices.DeleteFunc(rs, func(r result) bool { return parents[r.name] })
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// table prints a markdown table of per-package test time, one column per
// name=file.json run, then the slowest tests of the first run.
func table(out io.Writer, args []string) int {
	type col struct {
		name  string
		pkgs  map[string]time.Duration
		tests []result
	}
	var cols []col
	var order []string
	seen := map[string]bool{}
	for _, a := range args {
		name, file, ok := strings.Cut(a, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "usage: testbudget table name=file.json ...")
			return 2
		}
		f, err := os.Open(file)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		c := col{name: name, pkgs: map[string]time.Duration{}}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
		for sc.Scan() {
			var e event
			if json.Unmarshal(sc.Bytes(), &e) != nil || (e.Action != "pass" && e.Action != "fail") {
				continue
			}
			if e.Test == "" {
				c.pkgs[e.Package] = seconds(e.Elapsed)
				if !seen[e.Package] {
					seen[e.Package] = true
					order = append(order, e.Package)
				}
			} else {
				c.tests = append(c.tests, result{e.Package + " " + e.Test, seconds(e.Elapsed)})
			}
		}
		_ = f.Close()
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		return 0
	}
	slices.SortFunc(order, func(a, b string) int { return cmp.Compare(cols[0].pkgs[b], cols[0].pkgs[a]) })

	mod := commonPrefix(order)
	fmt.Fprintf(out, "| package |")
	for _, c := range cols {
		fmt.Fprintf(out, " %s |", c.name)
	}
	fmt.Fprintf(out, "\n|---|%s\n", strings.Repeat("--:|", len(cols)))
	totals := make([]time.Duration, len(cols))
	for _, p := range order {
		fmt.Fprintf(out, "| %s |", strings.TrimPrefix(p, mod))
		for i, c := range cols {
			if d, ok := c.pkgs[p]; ok {
				totals[i] += d
				fmt.Fprintf(out, " %.2fs |", d.Seconds())
			} else {
				fmt.Fprintf(out, " - |")
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "| **sum** |")
	for _, t := range totals {
		fmt.Fprintf(out, " **%.2fs** |", t.Seconds())
	}
	fmt.Fprintln(out)

	tests := leaves(cols[0].tests)
	slices.SortFunc(tests, func(a, b result) int { return cmp.Compare(b.elapsed, a.elapsed) })
	fmt.Fprintf(out, "\nSlowest tests (leaf tests and subtests, %s):\n\n| test | time |\n|---|--:|\n", cols[0].name)
	for _, t := range tests[:min(15, len(tests))] {
		fmt.Fprintf(out, "| %s | %.2fs |\n", strings.TrimPrefix(t.name, mod), t.elapsed.Seconds())
	}
	return 0
}

// commonPrefix is the module path shared by the packages ("" if fewer than two).
func commonPrefix(pkgs []string) string {
	if len(pkgs) < 2 {
		return ""
	}
	p := pkgs[0]
	for _, s := range pkgs[1:] {
		for !strings.HasPrefix(s, p) {
			p = p[:len(p)-1]
		}
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i+1]
	}
	return ""
}
