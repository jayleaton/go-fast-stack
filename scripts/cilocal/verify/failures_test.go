package verify_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

const module = "github.com/jayleaton/go-fast-stack"

func failureOf(lines []string, changed []string) *verify.Failure {
	failures := verify.NewFailures(module)
	for _, line := range lines {
		failures.Line(line)
	}
	return failures.Failure(changed)
}

func TestFailuresNameTestsAndWhereTheFixGoes(test *testing.T) {
	test.Parallel()
	goTest := []string{
		"=== RUN   TestCreateWidget",
		"    widgets_test.go:40: got 500",
		"--- FAIL: TestCreateWidget (0.01s)",
		"    --- FAIL: TestCreateWidget/empty_name (0.00s)",
		"--- FAIL: TestCreateWidget (0.01s)",
		"FAIL",
		"FAIL\tgithub.com/jayleaton/go-fast-stack/apps/web/internal/api\t1.2s",
		"ok  \tgithub.com/jayleaton/go-fast-stack/apps/web/internal/services\t0.4s",
	}
	apiTests := []string{
		"github.com/jayleaton/go-fast-stack/apps/web/internal/api TestCreateWidget",
		"github.com/jayleaton/go-fast-stack/apps/web/internal/api TestCreateWidget/empty_name",
	}
	lint := []string{"apps/web/x.go:12:3: Error return value is not checked (errcheck)", "1 issues:"}
	const inThisPR, forward = "fix it in this PR", "fix it forward in its own PR off main"
	for _, testCase := range []struct {
		name    string
		lines   []string
		changed []string
		tests   []string
		hint    string
	}{
		{"go test in a changed package", goTest, []string{"apps/web/internal/api/widgets.go"}, apiTests, "this branch changed apps/web/internal/api/widgets.go: " + inThisPR},
		{"go test elsewhere", goTest, []string{"apps/web/internal/services/widgets.go", "docs/CI.md"}, apiTests, forward},
		{"lint in a changed dir", lint, []string{"apps/web/y.go"}, []string{"apps/web/x.go"}, inThisPR},
		{"lint on the changed file", lint, []string{"apps/web/x.go"}, []string{"apps/web/x.go"}, "this branch changed apps/web/x.go: " + inThisPR},
		{"lint elsewhere", lint, []string{"apps/admin/x.go"}, []string{"apps/web/x.go"}, forward},
		{"coloured lint", []string{"\x1b[1m./apps/web/x.go:12:3:\x1b[0m msg (errcheck)"}, nil, []string{"apps/web/x.go"}, forward},
		{"tests without a package line", []string{"--- FAIL: TestOnly (0.00s)"}, []string{"apps/web/x.go"}, []string{"TestOnly"}, ""},
		{"build failure names the file", []string{"apps/web/x.go:3:1: undefined: y", "FAIL\tgithub.com/jayleaton/go-fast-stack/apps/web [build failed]"}, []string{"apps/web/x.go"}, []string{"apps/web/x.go"}, inThisPR},
		{"nothing named", []string{"make: *** [test] Error 1"}, []string{"apps/web/x.go"}, []string{}, "read the step's log: nothing in it names a test or file"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			failure := failureOf(testCase.lines, testCase.changed)
			if !slices.Equal(failure.Tests, testCase.tests) {
				test.Fatalf("Tests = %q, want %q", failure.Tests, testCase.tests)
			}
			if !strings.Contains(failure.Hint, testCase.hint) {
				test.Fatalf("Hint = %q, want it to mention %q", failure.Hint, testCase.hint)
			}
		})
	}
}

// go test prints each failing test before its package's FAIL line, so a test
// belongs to the next package that fails. Two failing packages must not name
// a test under the package it is not in.
func TestFailuresPairEachTestWithItsOwnPackage(test *testing.T) {
	test.Parallel()
	failure := failureOf([]string{
		"--- FAIL: TestCreateWidget (0.01s)",
		"FAIL\tgithub.com/jayleaton/go-fast-stack/apps/web/internal/api\t1.2s",
		"--- FAIL: TestDeleteUser (0.01s)",
		"FAIL\tgithub.com/jayleaton/go-fast-stack/packages/auth\t0.8s",
	}, nil)
	want := []string{
		"github.com/jayleaton/go-fast-stack/apps/web/internal/api TestCreateWidget",
		"github.com/jayleaton/go-fast-stack/packages/auth TestDeleteUser",
	}
	if !slices.Equal(failure.Tests, want) {
		test.Fatalf("Tests = %q, want %q", failure.Tests, want)
	}
}

func meminfo(available, swapTotal, swapFree string) string {
	return "MemTotal:       16000000 kB\nMemAvailable:   " + available + " kB\nSwapTotal:      " + swapTotal + " kB\nSwapFree:       " + swapFree + " kB\n"
}

func TestReadMeminfo(test *testing.T) {
	test.Parallel()
	got, err := verify.ReadMeminfo(meminfo("8388608", "2097152", "1048576"))
	if err != nil || got != (verify.Meminfo{Available: 8388608, SwapUsed: 1048576}) {
		test.Fatalf("ReadMeminfo = %+v, %v", got, err)
	}
	for _, testCase := range []struct {
		name, text, want string
	}{
		{"no MemAvailable", "SwapTotal: 1 kB\nSwapFree: 1 kB\n", "no MemAvailable"},
		{"no SwapTotal", "MemAvailable: 1 kB\nSwapFree: 1 kB\n", "no SwapTotal"},
		{"no SwapFree", "MemAvailable: 1 kB\nSwapTotal: 1 kB\n", "no SwapFree"},
		{"overflow", meminfo("99999999999999999999", "0", "0"), "MemAvailable"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if _, err := verify.ReadMeminfo(testCase.text); err == nil || !strings.Contains(err.Error(), testCase.want) {
				test.Fatalf("ReadMeminfo error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestMemoryPressure(test *testing.T) {
	test.Parallel()
	const gib = 1024 * 1024
	for _, testCase := range []struct {
		name          string
		before, after verify.Meminfo
		want          string
	}{
		{"plenty", verify.Meminfo{Available: 8 * gib}, verify.Meminfo{Available: 8 * gib, SwapUsed: verify.SwapClimbKiB - 1}, ""},
		{"exactly the floor", verify.Meminfo{}, verify.Meminfo{Available: verify.MinAvailableGiB * gib}, ""},
		{"starved", verify.Meminfo{}, verify.Meminfo{Available: 3 * gib}, "MemAvailable is 3.0 GiB, under 4 GiB"},
		{"swap climbing", verify.Meminfo{Available: 8 * gib, SwapUsed: gib}, verify.Meminfo{Available: 8 * gib, SwapUsed: gib + verify.SwapClimbKiB}, "swap use grew 256 MiB while sampling"},
		{"swap falling", verify.Meminfo{Available: 8 * gib, SwapUsed: 2 * gib}, verify.Meminfo{Available: 8 * gib, SwapUsed: gib}, ""},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			if got := verify.MemoryPressure(testCase.before, testCase.after, verify.MinAvailableGiB); got != testCase.want {
				test.Fatalf("MemoryPressure = %q, want %q", got, testCase.want)
			}
		})
	}
}
