package verify

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// The post-merge report (.github/workflows/post-merge-report.yml) comments
// on the merged pull request when a workflow that runs on a push to main
// fails there. It lists those workflows by name, so a new one must be added
// to it; ReportGaps names the ones it misses. The reader below knows only the
// workflow YAML this repository writes (block mappings, inline or dash lists).

var (
	onKey   = regexp.MustCompile(`^['"]?on['"]?:\s*(.*)$`)
	nameKey = regexp.MustCompile(`^name:\s*(.+)$`)
)

// yamlLine is a line without its comment, and its indent.
type yamlLine struct {
	indent int
	text   string
}

func yamlLines(text string) []yamlLine {
	lines := []yamlLine{}
	for _, raw := range strings.Split(text, "\n") {
		if hash := strings.Index(raw, " #"); hash != -1 {
			raw = raw[:hash]
		}
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		lines = append(lines, yamlLine{indent: len(raw) - len(strings.TrimLeft(raw, " ")), text: trimmed})
	}
	return lines
}

// children is the lines nested under lines[at].
func children(lines []yamlLine, at int) []yamlLine {
	end := at + 1
	for end < len(lines) && lines[end].indent > lines[at].indent {
		end++
	}
	return lines[at+1 : end]
}

// child is the index of `key:` among the direct children in `lines` (the
// least indented), and its inline value.
func child(lines []yamlLine, key string) (int, string, bool) {
	if len(lines) == 0 {
		return 0, "", false
	}
	indent := lines[0].indent
	for index, line := range lines {
		if line.indent != indent {
			continue
		}
		if value, found := strings.CutPrefix(line.text, key+":"); found {
			return index, strings.TrimSpace(value), true
		}
	}
	return 0, "", false
}

func unquote(text string) string { return strings.Trim(strings.TrimSpace(text), `'"`) }

// list is a key's value as a list: inline (`[a, b]` or a scalar) or the
// dash items nested under it.
func list(lines []yamlLine, at int, inline string) []string {
	items := []string{}
	if inline != "" {
		for _, item := range strings.Split(strings.Trim(inline, "[]"), ",") {
			if item = unquote(item); item != "" {
				items = append(items, item)
			}
		}
		return items
	}
	for _, line := range children(lines, at) {
		if item, found := strings.CutPrefix(line.text, "- "); found {
			items = append(items, unquote(item))
		}
	}
	return items
}

// WorkflowName is a workflow's top-level `name:`, or "".
func WorkflowName(text string) string {
	for _, line := range yamlLines(text) {
		if match := nameKey.FindStringSubmatch(line.text); line.indent == 0 && match != nil {
			return unquote(match[1])
		}
	}
	return ""
}

// PushesToMain is whether a workflow runs on a push to main: `on: push`, or
// a `push:` trigger whose branches (none listed: every branch) match main.
func PushesToMain(text string) bool {
	lines := yamlLines(text)
	for at, line := range lines {
		match := onKey.FindStringSubmatch(line.text)
		if line.indent != 0 || match == nil {
			continue
		}
		if match[1] != "" {
			return slices.Contains(list(nil, 0, match[1]), "push")
		}
		triggers := children(lines, at)
		push, inline, found := child(triggers, "push")
		if !found {
			return false
		}
		filters := children(triggers, push)
		if inline != "" || len(filters) == 0 {
			return true
		}
		branches, value, listed := child(filters, "branches")
		if !listed {
			_, _, tagsOnly := child(filters, "tags")
			return !tagsOnly
		}
		for _, pattern := range list(filters, branches, value) {
			if matched, _ := path.Match(pattern, Main); matched || pattern == "**" {
				return true
			}
		}
		return false
	}
	return false
}

// ReportedWorkflows is the `workflow_run.workflows` list of a workflow.
func ReportedWorkflows(text string) []string {
	lines := yamlLines(text)
	for at, line := range lines {
		if line.text != "workflow_run:" {
			continue
		}
		block := children(lines, at)
		if index, inline, found := child(block, "workflows"); found {
			return list(block, index, inline)
		}
	}
	return nil
}

// ReportGaps is the names of the workflows (file name to content) that run
// on a push to main but are missing from the post-merge report's list.
func ReportGaps(workflows map[string]string, report string) []string {
	reported := ReportedWorkflows(report)
	gaps := []string{}
	for _, text := range workflows {
		if name := WorkflowName(text); PushesToMain(text) && !slices.Contains(reported, name) {
			gaps = append(gaps, name)
		}
	}
	slices.Sort(gaps)
	return gaps
}

// TrustedTrigger is the only event ci.yml runs on.
const TrustedTrigger = "pull_request_target"

// TrustProblems is why a pull request workflow (ci.yml) would let the pull
// request decide its own required checks, or nothing. Its jobs are the gate,
// so they run the base's code on the head as data:
//
//   - only on pull_request_target: on pull_request GitHub takes the workflow
//     and its checkout from the pull request, so a branch or fork could
//     rewrite the job, or the code it runs (an init() that exits 0);
//   - no checkout of anything but the default, the base;
//   - no expression inside a run script, where a branch name or title would
//     run as shell; values go through env;
//   - the secret scan reads the head's objects (--head), so it scans the
//     head's files with the base's scanner and .gitleaks.toml;
//   - the attestation check knows the head's repository (HEAD_REPO), so a
//     fork's head fails closed.
func TrustProblems(text string) []string {
	lines := yamlLines(text)
	problems := []string{}
	if triggers := triggersOf(lines); !slices.Equal(triggers, []string{TrustedTrigger}) {
		problems = append(problems, fmt.Sprintf("it runs on %v, not %s alone: on pull_request the pull request supplies the workflow and the code it runs", triggers, TrustedTrigger))
	}
	for at, line := range lines {
		item, isItem := strings.CutPrefix(line.text, "- ")
		if !isItem {
			continue
		}
		step := append([]yamlLine{{indent: line.indent + len("- "), text: item}}, children(lines, at)...)
		problems = append(problems, stepTrustProblems(step)...)
	}
	return problems
}

// triggersOf is the events under a workflow's top-level `on:`.
func triggersOf(lines []yamlLine) []string {
	for at, line := range lines {
		match := onKey.FindStringSubmatch(line.text)
		if line.indent != 0 || match == nil {
			continue
		}
		if match[1] != "" {
			return list(nil, 0, match[1])
		}
		events := []string{}
		block := children(lines, at)
		for _, event := range block {
			if event.indent == block[0].indent {
				name, _, _ := strings.Cut(event.text, ":")
				events = append(events, unquote(name))
			}
		}
		return events
	}
	return nil
}

func stepTrustProblems(step []yamlLine) []string {
	problems := []string{}
	text := func(prefix string) bool {
		return slices.ContainsFunc(step, func(line yamlLine) bool { return strings.HasPrefix(line.text, prefix) })
	}
	mentions := func(fragment string) bool {
		return slices.ContainsFunc(step, func(line yamlLine) bool { return strings.Contains(line.text, fragment) })
	}
	if mentions("uses: actions/checkout") && (text("ref:") || text("repository:")) {
		problems = append(problems, "a checkout names a ref or repository: only the default checkout, the base, is trusted")
	}
	for at, line := range step {
		if !strings.HasPrefix(line.text, "run:") {
			continue
		}
		script := append([]yamlLine{line}, children(step, at)...)
		if slices.ContainsFunc(script, func(line yamlLine) bool { return strings.Contains(line.text, "${{") }) {
			problems = append(problems, fmt.Sprintf("%q puts an expression in its script: pass it through env", line.text))
		}
	}
	if mentions("scripts/cilocal secret-scan") && !mentions("--head") {
		problems = append(problems, "the secret scan does not read the head as data (--head)")
	}
	if mentions("scripts/cilocal check") && !text("HEAD_REPO:") {
		problems = append(problems, "the attestation check is not told the head's repository (HEAD_REPO), so a fork's head would not fail closed")
	}
	return problems
}
