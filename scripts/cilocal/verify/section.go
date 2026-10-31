package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// The PR body's "Local verification" section: rendered by make ci-local from
// the agent's statement of what it ran, and checked by the Local
// verification job in ci.yml (cilocal check), the merge script (cilocal
// merge) and the staging gate (cilocal attest). One module for both sides of
// the rule. make ci-local runs nothing: it tells the PR that the agent ran
// what CI would have checked, and says what that was.

// StatusContext is the commit status make ci-local posts on the head it attests.
const StatusContext = "local-ci"

// The section's markers in the PR body.
const (
	SectionStart = "<!-- local-verification:start -->"
	SectionEnd   = "<!-- local-verification:end -->"
)

// NextStep is the one fix for every problem the checks report.
const NextStep = "run what the change needs, then `make ci-local RAN=\"<what you ran>\"` on the pushed head (a new push or rebase needs a new attestation); it posts local-ci and fills the PR body"

// digestLength is how many hex characters of a sha256 the status carries.
const digestLength = 12

// shortLength is how many characters of a commit sha messages show.
const shortLength = 12

// descriptionLimit is the most GitHub keeps of a status description, in characters.
const descriptionLimit = 140

// Results of an attestation.
const (
	Passed = "passed"
	Failed = "failed"
)

// Short is a commit sha as messages show it.
func Short(sha string) string {
	if len(sha) > shortLength {
		return sha[:shortLength]
	}
	return sha
}

// StatementOf is a statement of what ran as one line (RAN may span lines,
// joined with "; "), or why it cannot be one: empty, or holding an HTML
// comment that could rewrite the section around it.
func StatementOf(text string) (string, string) {
	parts := []string{}
	for _, part := range strings.Split(text, "\n") {
		if part = strings.TrimSpace(part); part != "" {
			parts = append(parts, part)
		}
	}
	line := strings.Join(parts, "; ")
	switch {
	case line == "":
		return "", `say what you ran: make ci-local RAN="<commands>"`
	case strings.Contains(line, "<!--"):
		return "", "the statement may not hold an HTML comment"
	}
	return line, ""
}

// Report is one attestation: the head and its tree, who attested it where,
// when, the result, and the statements of what ran and (for a failure) what
// failed.
type Report struct {
	Head       string
	Tree       string
	Author     string
	Hostname   string
	FinishedAt string // RFC 3339
	Result     string // Passed or Failed
	Ran        string
	Failed     string // empty unless Result is Failed
}

// RenderSection is the section's inner Markdown for a report.
func RenderSection(report Report) string {
	lines := []string{
		fmt.Sprintf("> Filled by `make ci-local`, %s. Never edit this section by hand.", report.FinishedAt),
		"",
		fmt.Sprintf("Head `%s` · tree `%s` · %s", report.Head, Short(report.Tree), report.Result),
		fmt.Sprintf("Attested by `%s` on `%s`", report.Author, report.Hostname),
		"",
		"**Ran:** " + report.Ran,
	}
	if report.Failed != "" {
		lines = append(lines, "**Failed:** "+report.Failed)
	}
	return strings.Join(lines, "\n")
}

// SectionOf is the section's inner text, and false when the body has none.
func SectionOf(body string) (string, bool) {
	start := strings.Index(body, SectionStart)
	if start == -1 {
		return "", false
	}
	rest := body[start+len(SectionStart):]
	end := strings.Index(rest, SectionEnd)
	if end == -1 {
		return "", false
	}
	return strings.TrimSpace(rest[:end]), true
}

// SectionDigest is the digest `local-ci` carries for the section, so a
// section edited afterwards no longer matches.
func SectionDigest(section string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(section)))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// StatusDescription is the `local-ci` description: the result and the
// section's digest, before the author and host GitHub may cut.
func StatusDescription(report Report, section string) string {
	description := []rune(fmt.Sprintf("%s · body %s · by %s on %s", report.Result, SectionDigest(section), report.Author, report.Hostname))
	if len(description) > descriptionLimit {
		description = description[:descriptionLimit]
	}
	return string(description)
}

// WithSection replaces the section in a PR body, or adds it under a "Local
// verification" heading when the body has none. A start marker left without
// its end is dropped first, so SectionOf reads exactly the new section.
func WithSection(body, section string) string {
	block := SectionStart + "\n" + section + "\n" + SectionEnd
	start := strings.Index(body, SectionStart)
	if start != -1 {
		if end := strings.Index(body[start+len(SectionStart):], SectionEnd); end != -1 {
			after := start + len(SectionStart) + end + len(SectionEnd)
			return body[:start] + block + body[after:]
		}
		body = strings.ReplaceAll(body, SectionStart, "")
	}
	return strings.TrimRight(body, "\n ") + "\n\n## Local verification\n\n" + block + "\n"
}
