package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A step's inputs are the files a change to which, alone, would select it
// (Classify): the same rule that decides whether a branch runs the step at
// all. Its input hash covers those files' blobs at the head, its command and
// environment, and where it ran. When a later head of the same pull request
// has a step at the same hash as an earlier head that `local-ci` attested,
// make ci-local reuses it (docs/CI.md, "Local verification").

// hashLength is how many hex characters of a sha256 the ledger keeps.
const hashLength = 16

// alwaysRun are the steps never reused: their result depends on more than
// the tree (the secret scan's file list is the diff, the migration order
// reads the tips of the deployed branches), and both take seconds.
var alwaysRun = map[string]bool{JobSecrets: true, JobMigrations: true}

// Reusable is whether a step may be reused from an earlier head.
func Reusable(step Step) bool { return !alwaysRun[step.Job] }

// InputOf is whether a change to `file` alone would select the steps of `job`.
func InputOf(job, file string) bool {
	areas := Classify(file)
	switch job {
	case JobGo:
		return areas.Go
	case JobDeploy:
		return areas.Deploy
	case JobMobile:
		return areas.Mobile
	case JobWorkflows:
		return areas.Workflows
	}
	return true
}

// TreeEntry is one line of `git ls-tree -r -z <tree>`.
type TreeEntry struct {
	Mode, Blob, Path string
}

// ParseTree reads `git ls-tree -r -z` output.
func ParseTree(output string) []TreeEntry {
	entries := []TreeEntry{}
	for line := range strings.SplitSeq(output, "\x00") {
		meta, file, found := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		const ls = 3 // mode, type, object
		if !found || len(fields) != ls {
			continue
		}
		entries = append(entries, TreeEntry{Mode: fields[0], Blob: fields[2], Path: file})
	}
	return entries
}

// TreeDigest identifies a tree by its files (mode, blob, path), so a Davit
// box can prove it holds exactly the head's tree before any step runs.
func TreeDigest(entries []TreeEntry) string {
	sorted := append([]TreeEntry(nil), entries...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left].Path < sorted[right].Path })
	hash := sha256.New()
	for _, entry := range sorted {
		fmt.Fprintf(hash, "%s %s %s\n", entry.Mode, entry.Blob, entry.Path)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// InputHashes is each reusable step's input hash, by step id. `runner`
// names where the steps run (`local darwin/arm64`, `davit linux/amd64`), so
// a step proved on one platform is not reused for another.
func InputHashes(steps []Step, entries []TreeEntry, runner string) map[string]string {
	hashes := map[string]string{}
	for _, step := range steps {
		if !Reusable(step) {
			continue
		}
		hash := sha256.New()
		header, _ := json.Marshal(struct {
			Run    []string          `json:"run"`
			Env    map[string]string `json:"env"`
			Runner string            `json:"runner"`
		}{step.Run, step.Env, runner})
		hash.Write(header)
		for _, entry := range entries {
			if InputOf(step.Job, entry.Path) {
				fmt.Fprintf(hash, "%s %s %s\n", entry.Mode, entry.Blob, entry.Path)
			}
		}
		hashes[step.ID()] = hex.EncodeToString(hash.Sum(nil))[:hashLength]
	}
	return hashes
}

// LedgerStep is one passed step of a ledger: its id, its input hash, and for
// a step reused from an earlier head, that head.
type LedgerStep struct {
	ID, Hash, From string
}

// MarshalJSON writes the step as `[id, hash]` or `[id, hash, from]`.
func (step LedgerStep) MarshalJSON() ([]byte, error) {
	fields := []string{step.ID, step.Hash}
	if step.From != "" {
		fields = append(fields, step.From)
	}
	return json.Marshal(fields)
}

// UnmarshalJSON reads `[id, hash]` or `[id, hash, from]`.
func (step *LedgerStep) UnmarshalJSON(data []byte) error {
	var fields []string
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("ledger step: %w", err)
	}
	const short, long = 2, 3
	if len(fields) != short && len(fields) != long {
		return fmt.Errorf("ledger step has %d fields", len(fields))
	}
	step.ID, step.Hash = fields[0], fields[1]
	if len(fields) == long {
		step.From = fields[2]
	}
	return nil
}

// Ledger is the steps that passed on a head, with their input hashes. The
// section carries the run's own and those it reused steps from; `local-ci`
// carries its digest.
type Ledger struct {
	Head  string       `json:"head"`
	Steps []LedgerStep `json:"steps"`
}

// Lent is a step reused from an earlier head.
type Lent struct {
	From, Hash string
}

// Lenders are the earlier ledgers that may lend steps to `head`: attested by
// a successful `local-ci` on their head and never failed there.
// `statusesOf(sha)` is every status GitHub holds for that commit.
func Lenders(ledgers []Ledger, head string, statusesOf func(string) []Status) []Ledger {
	seen := map[string]bool{}
	lenders := []Ledger{}
	for _, ledger := range ledgers {
		if ledger.Head == head || seen[ledger.Head] {
			continue
		}
		seen[ledger.Head] = true
		if SourceProblem(ledger, statusesOf(ledger.Head)) == "" {
			lenders = append(lenders, ledger)
		}
	}
	return lenders
}

// Reuses is the steps `hashes` may take from `ledgers`: step id → the head
// it passed on with that hash.
func Reuses(hashes map[string]string, ledgers []Ledger) map[string]Lent {
	found := map[string]Lent{}
	for id, hash := range hashes {
		for _, ledger := range ledgers {
			if ledger.holds(id, hash) {
				found[id] = Lent{From: ledger.Head, Hash: hash}
				break
			}
		}
	}
	return found
}

func (ledger Ledger) holds(id, hash string) bool {
	for _, step := range ledger.Steps {
		if step.ID == id && step.Hash == hash {
			return true
		}
	}
	return false
}
