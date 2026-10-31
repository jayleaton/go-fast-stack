package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// The steps run the same way on this machine and in a Davit box: `execute`
// runs them in order in a checkout of the head and reports each one on a
// marker line (`::cilocal <kind> <json>`), so the side that selected them
// reads the same results either way.

const markerPrefix = "::cilocal "

// stepDone is the marker a finished step prints.
type stepDone struct {
	ID         string `json:"id"`
	Exit       int    `json:"exit"`
	DurationMS int64  `json:"durationMs"`
}

// payload is what the selecting side hands the executor.
type payload struct {
	Tree      string            `json:"tree"` // verify.TreeDigest of the head
	Steps     []verify.Step     `json:"steps"`
	Env       map[string]string `json:"env"`
	KeepGoing bool              `json:"keepGoing"`
	Affected  bool              `json:"affected,omitempty"` // run scripts/affected.sh with Env instead of the steps
	Paths     []string          `json:"paths,omitempty"`    // every file of the head's tree, for the box to prune what is not
}

// scrubbed are variables of the caller's shell that would point a step at
// another database or compose project than its own.
var scrubbed = []string{"TEST_DATABASE_URL", "DATABASE_URL", "ADMIN_DATABASE_URL", "COMPOSE_PROJECT_NAME",
	"POSTGRES_PORT", "PGWEB_PORT", "TEST_POSTGRES_PORT", "GOFLAGS", "AFFECTED_FULL", "AFFECTED_FILES", "CHECK_BASE", "GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE"}

func stepEnv(extra map[string]string) []string {
	env := []string{}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		keep := true
		for _, drop := range scrubbed {
			keep = keep && name != drop
		}
		if keep {
			env = append(env, entry)
		}
	}
	for name, value := range extra {
		env = append(env, name+"="+value)
	}
	return env
}

func marker(writer io.Writer, kind string, value any) {
	encoded, _ := json.Marshal(value)
	fmt.Fprintf(writer, "%s%s %s\n", markerPrefix, kind, encoded)
}

// execute runs the steps in `dir`, streaming their output to `out`, and
// stops at the first failure unless KeepGoing. It returns whether all passed.
func execute(ctx context.Context, dir string, work payload, out io.Writer) bool {
	passed := true
	for _, step := range work.Steps {
		if !passed && !work.KeepGoing {
			break
		}
		marker(out, "start", step.ID())
		started := time.Now()
		command := exec.CommandContext(ctx, step.Run[0], step.Run[1:]...)
		command.Dir = dir
		env := map[string]string{}
		for name, value := range work.Env {
			env[name] = value
		}
		for name, value := range step.Env {
			env[name] = value
		}
		command.Env = stepEnv(env)
		command.Stdout, command.Stderr = out, out
		exit := 0
		if err := command.Run(); err != nil {
			exit = -1
			if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
				exit = exitErr.ExitCode()
			} else {
				fmt.Fprintf(out, "%s: %v\n", step.ID(), err)
			}
		}
		passed = passed && exit == 0
		marker(out, "done", stepDone{ID: step.ID(), Exit: exit, DurationMS: time.Since(started).Milliseconds()})
	}
	return passed
}

// worktreeDigest is verify.TreeDigest of the files in `dir` as they are on
// disk: every tracked or untracked file git does not ignore, with git's
// mode and blob id. A box proves with it that it holds exactly the head.
func worktreeDigest(ctx context.Context, dir string) (string, error) {
	listed, err := git(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	entries := []verify.TreeEntry{}
	seen := map[string]bool{}
	for file := range strings.SplitSeq(listed, "\x00") {
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		entry, present, err := fileEntry(filepath.Join(dir, file))
		if err != nil {
			return "", err
		}
		if present {
			entry.Path = file
			entries = append(entries, entry)
		}
	}
	return verify.TreeDigest(entries), nil
}

const (
	modeFile       = "100644"
	modeExecutable = "100755"
	modeSymlink    = "120000"
	executableBits = 0o111
)

func fileEntry(file string) (verify.TreeEntry, bool, error) {
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) {
		return verify.TreeEntry{}, false, nil // deleted in the working tree
	}
	if err != nil {
		return verify.TreeEntry{}, false, fmt.Errorf("tree: %w", err)
	}
	var content []byte
	mode := modeFile
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(file)
		if err != nil {
			return verify.TreeEntry{}, false, fmt.Errorf("tree: %w", err)
		}
		content, mode = []byte(target), modeSymlink
	case info.IsDir():
		return verify.TreeEntry{}, false, nil // a submodule or nested repository
	default:
		if content, err = os.ReadFile(file); err != nil {
			return verify.TreeEntry{}, false, fmt.Errorf("tree: %w", err)
		}
		if info.Mode()&executableBits != 0 {
			mode = modeExecutable
		}
	}
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", len(content))
	hash.Write(content)
	return verify.TreeEntry{Mode: mode, Blob: hex.EncodeToString(hash.Sum(nil)), Path: ""}, true, nil
}

// payloadEnv carries the payload into the box, gzipped JSON in base64, so
// nothing is added to the synced tree.
const payloadEnv = "CILOCAL_PAYLOAD"

func encodePayload(work payload) (string, error) {
	encoded, err := json.Marshal(work)
	if err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	var buffer bytes.Buffer
	zipped := gzip.NewWriter(&buffer)
	if _, err := zipped.Write(encoded); err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	if err := zipped.Close(); err != nil {
		return "", fmt.Errorf("payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buffer.Bytes()), nil
}

func decodePayload(text string) (payload, error) {
	var work payload
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return work, fmt.Errorf("payload: %w", err)
	}
	zipped, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return work, fmt.Errorf("payload: %w", err)
	}
	if err := json.NewDecoder(zipped).Decode(&work); err != nil {
		return work, fmt.Errorf("payload: %w", err)
	}
	return work, nil
}

// pruneStale deletes the files of `dir` that are not in the head's tree:
// Davit's sync adds and updates files but never deletes, so a box made from
// a project image built at an older commit still holds files the head
// removed. It returns how many it deleted.
func pruneStale(ctx context.Context, dir string, paths []string) (int, error) {
	keep := map[string]bool{}
	for _, file := range paths {
		keep[file] = true
	}
	listed, err := git(ctx, dir, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return 0, err
	}
	pruned := 0
	for file := range strings.SplitSeq(listed, "\x00") {
		if file == "" || keep[file] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return pruned, fmt.Errorf("prune: %w", err)
		}
		if _, err := git(ctx, dir, "rm", "--cached", "--quiet", "--ignore-unmatch", "--", file); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

// execCommand is `cilocal exec`: the executor inside a Davit box. It refuses
// to run anything unless the box's tree is the head's, runs the steps, then
// reports the tree as it is afterwards (a step that rewrote a tracked file
// is a failure the selecting side reports).
func execCommand(ctx context.Context, _ []string) error {
	work, err := decodePayload(os.Getenv(payloadEnv))
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	pruned, err := pruneStale(ctx, dir, work.Paths)
	if err != nil {
		return err
	}
	if pruned > 0 {
		fmt.Fprintf(os.Stderr, "cilocal exec: removed %d files the head does not have (left by the box's image)\n", pruned)
	}
	before, err := worktreeDigest(ctx, dir)
	if err != nil {
		return err
	}
	marker(os.Stdout, "tree", before)
	if before != work.Tree {
		return fmt.Errorf("the box's tree %s is not the head's %s: nothing ran", verify.Short(before), verify.Short(work.Tree))
	}
	if work.Affected {
		lines, err := affectedIn(ctx, dir, work.Env)
		if err != nil {
			return err
		}
		marker(os.Stdout, "affected", lines)
		return nil
	}
	execute(ctx, dir, work, os.Stdout)
	after, err := worktreeDigest(ctx, dir)
	if err != nil {
		return err
	}
	marker(os.Stdout, "after", after)
	return nil
}

// results reads the executor's output: it echoes every line to `echo`,
// feeds each step's lines to a verify.Failures, and returns the finished
// steps, the tree digests the box reported and the failures by step id.
type results struct {
	done     map[string]stepDone
	tree     string
	after    string
	affected string
	failures map[string]*verify.Failures
}

func readResults(reader io.Reader, echo io.Writer, module string) results {
	found := results{done: map[string]stepDone{}, tree: "", after: "", affected: "", failures: map[string]*verify.Failures{}}
	scanner := bufio.NewScanner(reader)
	const maxLine = 1 << 20
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxLine)
	current := ""
	for scanner.Scan() {
		line := scanner.Text()
		rest, isMarker := strings.CutPrefix(line, markerPrefix)
		if !isMarker {
			fmt.Fprintln(echo, line)
			if current != "" {
				found.failures[current].Line(line)
			}
			continue
		}
		kind, value, _ := strings.Cut(rest, " ")
		switch kind {
		case "start":
			_ = json.Unmarshal([]byte(value), &current)
			found.failures[current] = verify.NewFailures(module)
			fmt.Fprintf(echo, "\n==> %s\n", current)
		case "done":
			var done stepDone
			if json.Unmarshal([]byte(value), &done) == nil {
				found.done[done.ID] = done
				fmt.Fprintf(echo, "<== %s: exit %d, %s\n", done.ID, done.Exit, verify.Duration(done.DurationMS))
			}
			current = ""
		case "tree":
			_ = json.Unmarshal([]byte(value), &found.tree)
		case "after":
			_ = json.Unmarshal([]byte(value), &found.after)
		case "affected":
			_ = json.Unmarshal([]byte(value), &found.affected)
		}
	}
	return found
}
