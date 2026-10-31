package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The PR body's "Local verification" section: rendered by make ci-local
// from its run report, and checked by the Local verification job in ci.yml
// (cilocal check), the merge script (cilocal merge) and the staging gate
// (cilocal attest). One module for both sides of the rule.

// StatusContext is the commit status make ci-local posts on the head it verified.
const StatusContext = "local-ci"

// The section's markers in the PR body.
const (
	SectionStart = "<!-- local-verification:start -->"
	SectionEnd   = "<!-- local-verification:end -->"
)

// NextStep is the one fix for every problem the checks report.
const NextStep = "run `make ci-local` on the pushed head (a new push or rebase needs a new run); it posts local-ci and fills the PR body"

// digestLength is how many hex characters of a sha256 the status carries.
const digestLength = 12

// shortLength is how many characters of a commit sha messages show.
const shortLength = 12

// descriptionLimit is the most GitHub keeps of a status description.
const descriptionLimit = 140

// Results of a run.
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

// Duration formats a step's time: `12s`, `1m02s`.
func Duration(milliseconds int64) string {
	seconds := (time.Duration(milliseconds)*time.Millisecond + time.Second/2) / time.Second
	const minute = 60
	if seconds < minute {
		return fmt.Sprintf("%ds", seconds)
	}
	return fmt.Sprintf("%dm%02ds", seconds/minute, seconds%minute)
}

// Failure is what a failed step printed that names tests or files, and where
// the fix goes (Failures).
type Failure struct {
	Tests []string `json:"tests"`
	Hint  string   `json:"hint"`
}

// StepResult is a step of the report: nil Exit when it did not run (an
// earlier step failed), and Reused when an earlier head lent it.
type StepResult struct {
	Step
	InputHash  string   `json:"inputHash,omitempty"`
	Exit       *int     `json:"exit"`
	DurationMS int64    `json:"durationMs"`
	Reused     *Lent    `json:"reused,omitempty"`
	Failure    *Failure `json:"failure,omitempty"`
}

// Report is one run of make ci-local.
type Report struct {
	Head         string            `json:"head"`
	Tree         string            `json:"tree"`
	MergeBase    string            `json:"mergeBase"`
	Base         string            `json:"base"` // what the merge-base is taken with: origin/main, or the last attested main commit
	Host         string            `json:"host"` // where the steps ran: the machine's name, or davit:<project>/<box>
	Versions     map[string]string `json:"versions"`
	Areas        []string          `json:"areas"`
	ChangedFiles int               `json:"changedFiles"`
	Steps        []StepResult      `json:"steps"`
	Sources      []Ledger          `json:"sources,omitempty"` // the earlier ledgers it reused steps from
	FinishedAt   string            `json:"finishedAt"`
	DurationMS   int64             `json:"durationMs"`
	Result       string            `json:"result"`
}

// LedgerOf is the report's ledger: every step that passed, with its input
// hash, and for a reused step the head it came from.
func LedgerOf(report Report) Ledger {
	steps := []LedgerStep{}
	for _, step := range report.Steps {
		if step.Exit == nil || *step.Exit != 0 || step.InputHash == "" {
			continue
		}
		entry := LedgerStep{ID: step.ID(), Hash: step.InputHash, From: ""}
		if step.Reused != nil {
			entry.From = step.Reused.From
		}
		steps = append(steps, entry)
	}
	return Ledger{Head: report.Head, Steps: steps}
}

func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:digestLength]
}

// LedgerDigest is the digest `local-ci` carries for a ledger.
func LedgerDigest(ledger Ledger) string {
	encoded, _ := json.Marshal(ledger)
	return digest(string(encoded))
}

// SectionDigest is the digest `local-ci` carries for the section, so a
// section edited afterwards no longer matches.
func SectionDigest(section string) string { return digest(strings.TrimSpace(section)) }

const ledgerMarker = "<!-- ci-local-%s %s -->"

var ledgerLine = regexp.MustCompile(`(?m)^<!-- ci-local-(ledger|source) (\{.*\}) -->$`)

// Ledgers reads the section's own ledger and the earlier ones it reused
// steps from.
func Ledgers(section string) (*Ledger, []Ledger) {
	var own *Ledger
	sources := []Ledger{}
	for _, match := range ledgerLine.FindAllStringSubmatch(section, -1) {
		var ledger Ledger
		if err := json.Unmarshal([]byte(match[2]), &ledger); err != nil || ledger.Head == "" {
			continue
		}
		if match[1] == "ledger" {
			own = &ledger
		} else {
			sources = append(sources, ledger)
		}
	}
	return own, sources
}

// ReusedSources is the earlier heads a body's own ledger reuses steps from.
func ReusedSources(body string) []string {
	section, _ := SectionOf(body)
	own, _ := Ledgers(section)
	if own == nil {
		return nil
	}
	heads := []string{}
	seen := map[string]bool{}
	for _, step := range own.Steps {
		if step.From != "" && !seen[step.From] {
			seen[step.From] = true
			heads = append(heads, step.From)
		}
	}
	return heads
}

// RenderSection is the section's inner Markdown for a report: its head, tree
// and merge-base, where it ran, the selected areas, and one line per
// checklist item with every step's exit code and time.
func RenderSection(report Report) string {
	areas := "none (secret scan only)"
	if len(report.Areas) > 0 {
		areas = "`" + strings.Join(report.Areas, "`, `") + "`"
	}
	versions := []string{}
	for _, name := range []string{"go", "docker", "runner"} {
		if value := report.Versions[name]; value != "" {
			versions = append(versions, name+" "+value)
		}
	}
	lines := []string{
		fmt.Sprintf("> Filled by `make ci-local` from its run report, %s. Never edit this section or tick a box by hand.", report.FinishedAt),
		"",
		fmt.Sprintf("Head `%s` · tree `%s` · merge-base `%s` (%s)", report.Head, Short(report.Tree), Short(report.MergeBase), report.Base),
		fmt.Sprintf("Ran on `%s`: %s · %s · %s", report.Host, strings.Join(versions, ", "), Duration(report.DurationMS), report.Result),
		fmt.Sprintf("Areas: %s · %d changed files", areas, report.ChangedFiles),
		"",
	}
	for _, item := range Items {
		lines = append(lines, itemLines(item, report.Steps)...)
	}
	encoded, _ := json.Marshal(LedgerOf(report))
	lines = append(lines, "", fmt.Sprintf(ledgerMarker, "ledger", encoded))
	for _, source := range report.Sources {
		encoded, _ := json.Marshal(source)
		lines = append(lines, fmt.Sprintf(ledgerMarker, "source", encoded))
	}
	return strings.Join(lines, "\n")
}

// maxNamedTests is how many failing tests a checklist line names.
const maxNamedTests = 3

func itemLines(item string, all []StepResult) []string {
	steps := []StepResult{}
	for _, step := range all {
		if step.Item == item {
			steps = append(steps, step)
		}
	}
	if len(steps) == 0 {
		return []string{fmt.Sprintf("- [x] **%s**: not selected", item)}
	}
	ok, failed := true, false
	details := []string{}
	for _, step := range steps {
		switch {
		case step.Exit == nil:
			ok = false
			details = append(details, fmt.Sprintf("`%s` not run", step.Name))
		case step.Reused != nil:
			details = append(details, fmt.Sprintf("`%s` reused from `%s` (inputs `%s`)", step.Name, Short(step.Reused.From), step.InputHash))
		default:
			ok = ok && *step.Exit == 0
			failed = failed || *step.Exit != 0
			details = append(details, fmt.Sprintf("`%s` exit %d, %s", step.Name, *step.Exit, Duration(step.DurationMS)))
		}
	}
	state, mark := "passed", "x"
	if !ok {
		mark = " "
		state = "not run (an earlier step failed)"
		if failed {
			state = "FAILED"
		}
	}
	lines := []string{fmt.Sprintf("- [%s] **%s**: %s: %s", mark, item, state, strings.Join(details, "; "))}
	for _, step := range steps {
		if step.Failure == nil || step.Exit == nil || *step.Exit == 0 {
			continue
		}
		named := ""
		if tests := step.Failure.Tests; len(tests) > 0 {
			shown := tests[:min(len(tests), maxNamedTests)]
			named = "`" + strings.Join(shown, "`; `") + "`"
			if len(tests) > maxNamedTests {
				named += fmt.Sprintf(" and %d more", len(tests)-maxNamedTests)
			}
			named += ": "
		}
		lines = append(lines, fmt.Sprintf("  - `%s`: %s%s", step.Name, named, step.Failure.Hint))
	}
	return lines
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

// StatusDescription is the `local-ci` description: the result, the
// section's and the ledger's digests, then the host (which GitHub may cut).
func StatusDescription(report Report, section string) string {
	description := fmt.Sprintf("%s in %s · body %s · ledger %s · on %s",
		report.Result, Duration(report.DurationMS), SectionDigest(section), LedgerDigest(LedgerOf(report)), report.Host)
	if len(description) > descriptionLimit {
		description = description[:descriptionLimit]
	}
	return description
}
