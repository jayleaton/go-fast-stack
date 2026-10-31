package verify_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

func TestShort(test *testing.T) {
	test.Parallel()
	if got := verify.Short(headSHA); got != strings.Repeat("a", 12) {
		test.Fatalf("Short = %q", got)
	}
	if got := verify.Short("abc"); got != "abc" {
		test.Fatalf("Short of a short sha = %q", got)
	}
}

func TestStatementOf(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name, text, line, problem string
	}{
		{"one line", "make lint test", "make lint test", ""},
		{"lines joined", "  make lint\n\n make test TEST_PKGS=./x \n", "make lint; make test TEST_PKGS=./x", ""},
		{"empty", "", "", "say what you ran"},
		{"blank lines", " \n\t\n", "", "say what you ran"},
		{"html comment", "make lint <!-- local-verification:end -->", "", "may not hold an HTML comment"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			line, problem := verify.StatementOf(testCase.text)
			if line != testCase.line || !strings.Contains(problem, testCase.problem) || (testCase.problem == "") != (problem == "") {
				test.Fatalf("StatementOf(%q) = %q, %q; want %q, %q", testCase.text, line, problem, testCase.line, testCase.problem)
			}
		})
	}
}

func TestRenderSection(test *testing.T) {
	test.Parallel()
	want := strings.Join([]string{
		"> Filled by `make ci-local`, 2026-10-31T10:00:00Z. Never edit this section by hand.",
		"",
		"Head `" + headSHA + "` · tree `dddddddddddd` · passed",
		"Attested by `JJ Eaton` on `laptop`",
		"",
		"**Ran:** make lint test TEST_PKGS=./scripts/cilocal/verify",
	}, "\n")
	if got := verify.RenderSection(passingReport()); got != want {
		test.Fatalf("RenderSection =\n%s\nwant\n%s", got, want)
	}
	failed := passingReport()
	failed.Result, failed.Failed = verify.Failed, "make test: TestX"
	got := verify.RenderSection(failed)
	if !strings.Contains(got, "· failed\n") || !strings.HasSuffix(got, "\n**Failed:** make test: TestX") {
		test.Fatalf("a failed section = %q", got)
	}
}

func TestSectionRoundTripAttestsTheHead(test *testing.T) {
	test.Parallel()
	section := verify.RenderSection(passingReport())
	for _, testCase := range []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"template body", "## Problem\n\nWhat changed.\n\n"},
		{"existing section", "## Problem\n\n## Local verification\n\n" + verify.SectionStart + "\nstale\n" + verify.SectionEnd + "\n\n## Notes\nkept"},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			body := verify.WithSection(testCase.body, section)
			got, found := verify.SectionOf(body)
			if !found || got != strings.TrimSpace(section) {
				test.Fatalf("SectionOf = %q, %v; want the rendered section", got, found)
			}
			requireNone(test, verify.BodyProblems(body, headSHA))
			if strings.Contains(body, "stale") {
				test.Fatal("WithSection kept the old section")
			}
			if strings.Count(body, verify.SectionStart) != 1 {
				test.Fatalf("body has %d sections", strings.Count(body, verify.SectionStart))
			}
			if strings.Contains(testCase.body, "## Notes") && !strings.HasSuffix(body, "## Notes\nkept") {
				test.Fatalf("WithSection dropped the text after the section: %q", body)
			}
		})
	}
}

// A body whose section lost its end marker (an edit that cut it) gets a new
// section appended, and SectionOf must read that new section back: make
// ci-local posts the digest of what it rendered, and the check digests what
// SectionOf reads.
func TestWithSectionRoundTripsABodyWithADanglingStartMarker(test *testing.T) {
	test.Parallel()
	section := verify.RenderSection(passingReport())
	body := verify.WithSection("## Problem\n\n"+verify.SectionStart+"\nstale, end marker deleted", section)
	got, found := verify.SectionOf(body)
	if !found || got != strings.TrimSpace(section) {
		test.Fatalf("SectionOf(WithSection(body, section)) = %q, want the rendered section", got)
	}
}

func TestSectionOfWithoutMarkers(test *testing.T) {
	test.Parallel()
	for _, body := range []string{"", "no markers", verify.SectionStart + " no end", verify.SectionEnd + " end only"} {
		test.Run(body, func(test *testing.T) {
			test.Parallel()
			if section, found := verify.SectionOf(body); found || section != "" {
				test.Fatalf("SectionOf(%q) = %q, %v", body, section, found)
			}
		})
	}
}

func TestStatusDescription(test *testing.T) {
	test.Parallel()
	report := passingReport()
	section := verify.RenderSection(report)
	want := "passed · body " + verify.SectionDigest(section) + " · by JJ Eaton on laptop"
	if got := verify.StatusDescription(report, section); got != want {
		test.Fatalf("StatusDescription = %q, want %q", got, want)
	}
	if verify.SectionDigest(section) != verify.SectionDigest("\n"+section+"\n ") || len(verify.SectionDigest(section)) != 12 {
		test.Fatal("SectionDigest is not a 12-character digest of the trimmed section")
	}
	report.Hostname = strings.Repeat("é", 200)
	long := verify.StatusDescription(report, section)
	if utf8.RuneCountInString(long) != 140 || !utf8.ValidString(long) || !strings.HasPrefix(long, "passed · body ") {
		test.Fatalf("a long description = %q (%d characters)", long, utf8.RuneCountInString(long))
	}
}
