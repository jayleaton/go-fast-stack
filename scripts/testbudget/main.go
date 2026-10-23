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
// `make bench-devloop`, one column per run (a comma-separated list of
// repeated runs gives each package's median):
//
//	testbudget table race=race.json norace=norace.json
//	testbudget table base=b1.json,b2.json,b3.json head=h1.json,h2.json,h3.json
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

// Default budgets, about twice the slowest package (~25 s) and test (~5 s,
// CPU-bound scrypt) seen on CI under -race while the cold build cache also
// compiles: tight enough to catch a doubling, loose enough not to flake.
// Raise one only with a reason in the commit message.
const (
	defaultPackageBudget = 60 * time.Second
	defaultTestBudget    = 10 * time.Second
)

const (
	defaultTop    = 10      // slowest tests listed after a run
	tableTop      = 15      // slowest tests listed under a table
	exitUsage     = 2       // conventional exit status for a command-line misuse
	scanInitial   = 1 << 20 // 1 MiB: initial line buffer for go test -json
	scanMaxLine   = 1 << 26 // 64 MiB: longest -json line accepted (huge test output)
	minPrefixPkgs = 2       // a common module prefix needs at least two packages
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
	top := flag.Int("top", defaultTop, "list this many slowest tests at the end")
	flag.Parse()
	os.Exit(run(os.Stdin, os.Stdout, *pkgBudget, *testBudget, *top))
}

type result struct {
	name    string // package, or "package Test"
	elapsed time.Duration
}

// run streams a go test -json run and returns the exit code.
func run(in io.Reader, out io.Writer, pkgBudget, testBudget time.Duration, top int) int {
	writer := bufio.NewWriter(out)
	defer func() { _ = writer.Flush() }()

	type key struct{ pkg, test string }
	logs := map[key][]string{}
	var pkgs, tests []result
	failed := false

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, scanInitial), scanMaxLine)
	for sc.Scan() {
		line := sc.Bytes()
		var parsed event
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &parsed) != nil {
			fmt.Fprintf(writer, "%s\n", line) // not test2json: pass through
			continue
		}
		logKey := key{parsed.Package, parsed.Test}
		switch parsed.Action {
		case "build-output":
			fmt.Fprint(writer, parsed.Output)
		case "build-fail":
			failed = true
		case "output":
			if !strings.HasPrefix(parsed.Output, "=== ") {
				logs[logKey] = append(logs[logKey], parsed.Output)
			}
		case "pass", "skip", "fail":
			lines := logs[logKey]
			delete(logs, logKey)
			duration := seconds(parsed.Elapsed)
			if parsed.Action == "fail" {
				failed = true
				for _, line := range lines {
					fmt.Fprint(writer, line)
				}
				break
			}
			if parsed.Test != "" {
				if parsed.Action == "pass" {
					tests = append(tests, result{parsed.Package + " " + parsed.Test, duration})
				}
				break
			}
			// A passing package prints only its summary, as plain go test does.
			for _, line := range lines {
				if strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "?") || strings.HasPrefix(line, "\t") {
					fmt.Fprint(writer, line)
				}
			}
			if parsed.Action == "pass" {
				pkgs = append(pkgs, result{parsed.Package, duration})
			}
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(writer, "testbudget: read:", err)
		return 1
	}

	byTime := func(left, right result) int { return cmp.Compare(right.elapsed, left.elapsed) }
	tests = leaves(tests)
	slices.SortFunc(tests, byTime)
	slices.SortFunc(pkgs, byTime)
	if top > 0 && len(tests) > 0 {
		fmt.Fprintf(writer, "\nslowest tests:\n")
		for _, result := range tests[:min(top, len(tests))] {
			fmt.Fprintf(writer, "  %7.2fs  %s\n", result.elapsed.Seconds(), result.name)
		}
	}

	over := overBudget(pkgs, pkgBudget, "package")
	over = append(over, overBudget(tests, testBudget, "test")...)
	if len(over) > 0 {
		fmt.Fprintf(writer, "\nFAIL  over the test-time budget:\n")
		for _, origin := range over {
			fmt.Fprintf(writer, "  %s\n", origin)
		}
		fmt.Fprintf(writer, "make it fast (AGENTS.md \"Fast tests\"): t.Parallel(), cloned databases, fake clocks, no sleeps\n")
		failed = true
	}
	if failed {
		return 1
	}
	return 0
}

func overBudget(rs []result, budget time.Duration, kind string) []string {
	var over []string
	for _, result := range rs {
		if budget > 0 && result.elapsed > budget {
			over = append(over, fmt.Sprintf("%s %s: %.2fs (budget %s)", kind, result.name, result.elapsed.Seconds(), budget))
		}
	}
	return over
}

// leaves drops tests that have subtests ("pkg Test" when "pkg Test/x" ran).
func leaves(rs []result) []result {
	parents := map[string]bool{}
	for _, result := range rs {
		for index := len(result.name) - 1; index > 0; index-- {
			if result.name[index] == '/' {
				parents[result.name[:index]] = true
			}
		}
	}
	return slices.DeleteFunc(rs, func(result result) bool { return parents[result.name] })
}

func seconds(secs float64) time.Duration { return time.Duration(secs * float64(time.Second)) }

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
	for _, arg := range args {
		name, files, ok := strings.Cut(arg, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "usage: testbudget table name=file.json[,file.json...] ...")
			return exitUsage
		}
		// Several files (repeated runs): each package's and test's median.
		pkgRuns, testRuns := map[string][]time.Duration{}, map[string][]time.Duration{}
		for file := range strings.SplitSeq(files, ",") {
			handle, err := os.Open(file)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			sc := bufio.NewScanner(handle)
			sc.Buffer(make([]byte, 0, scanInitial), scanMaxLine)
			for sc.Scan() {
				var parsed event
				if json.Unmarshal(sc.Bytes(), &parsed) != nil || (parsed.Action != "pass" && parsed.Action != "fail") {
					continue
				}
				if parsed.Test == "" {
					pkgRuns[parsed.Package] = append(pkgRuns[parsed.Package], seconds(parsed.Elapsed))
					if !seen[parsed.Package] {
						seen[parsed.Package] = true
						order = append(order, parsed.Package)
					}
				} else {
					key := parsed.Package + " " + parsed.Test
					testRuns[key] = append(testRuns[key], seconds(parsed.Elapsed))
				}
			}
			_ = handle.Close()
		}
		column := col{name: name, pkgs: map[string]time.Duration{}}
		for key, ds := range pkgRuns {
			column.pkgs[key] = median(ds)
		}
		for key, ds := range testRuns {
			column.tests = append(column.tests, result{key, median(ds)})
		}
		cols = append(cols, column)
	}
	if len(cols) == 0 {
		return 0
	}
	slices.SortFunc(order, func(left, right string) int { return cmp.Compare(cols[0].pkgs[right], cols[0].pkgs[left]) })

	mod := commonPrefix(order)
	fmt.Fprintf(out, "| package |")
	for _, col := range cols {
		fmt.Fprintf(out, " %s |", col.name)
	}
	fmt.Fprintf(out, "\n|---|%s\n", strings.Repeat("--:|", len(cols)))
	totals := make([]time.Duration, len(cols))
	for _, path := range order {
		fmt.Fprintf(out, "| %s |", strings.TrimPrefix(path, mod))
		for index, col := range cols {
			if duration, ok := col.pkgs[path]; ok {
				totals[index] += duration
				fmt.Fprintf(out, " %.2fs |", duration.Seconds())
			} else {
				fmt.Fprintf(out, " - |")
			}
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "| **sum** |")
	for _, total := range totals {
		fmt.Fprintf(out, " **%.2fs** |", total.Seconds())
	}
	fmt.Fprintln(out)

	tests := leaves(cols[0].tests)
	slices.SortFunc(tests, func(left, right result) int { return cmp.Compare(right.elapsed, left.elapsed) })
	fmt.Fprintf(out, "\nSlowest tests (leaf tests and subtests, %s):\n\n| test | time |\n|---|--:|\n", cols[0].name)
	for _, result := range tests[:min(tableTop, len(tests))] {
		fmt.Fprintf(out, "| %s | %.2fs |\n", strings.TrimPrefix(result.name, mod), result.elapsed.Seconds())
	}
	return 0
}

func median(ds []time.Duration) time.Duration {
	slices.Sort(ds)
	return ds[len(ds)/2]
}

// commonPrefix is the module path shared by the packages ("" if fewer than two).
func commonPrefix(pkgs []string) string {
	if len(pkgs) < minPrefixPkgs {
		return ""
	}
	path := pkgs[0]
	for _, text := range pkgs[1:] {
		for !strings.HasPrefix(text, path) {
			path = path[:len(path)-1]
		}
	}
	if index := strings.LastIndex(path, "/"); index >= 0 {
		return path[:index+1]
	}
	return ""
}
