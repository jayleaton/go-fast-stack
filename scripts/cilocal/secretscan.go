package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// The secret scan (docs/CI.md, "Secret scan"): the files a change touches,
// copied out and scanned with gitleaks (pinned, checksum-verified), using
// .gitleaks.toml. make secret-scan runs it on the branch's changed files; the
// Secret scan job in ci.yml runs it on the pull request's file list, so it
// does not depend on the agent's machine. A finding fails, printing the
// rule, file and line with the value redacted.
//
//	cilocal secret-scan --base <commit>   files changed since the commit
//	cilocal secret-scan --pr <number>     the pull request's files (GitHub's list)
//	cilocal secret-scan --all             every tracked file
//	cilocal secret-scan <file>...         just these files
//
// --head <sha> (the Secret scan job) reads those files from that commit's
// objects instead of the checkout: the job checks out the base, so the
// scanner is the base's and the pull request's head is only data
// (verify.ScanTargets), never checked out or run.
//
// Policy (verify.ScannerPolicy: .gitleaks.toml and .gitleaksignore) comes
// only from the trusted commit: the --base commit, or else the checkout's
// HEAD (the base, in the job). It is written outside the scanned directory,
// a change's own policy files are copied in as content under another name
// (verify.ScanCopyPath), inline gitleaks:allow comments and GITLEAKS_* env
// are ignored, and a change that edits policy fails for review
// (verify.PolicyProblem) after the scan.

const gitleaksVersion = "8.30.1"

// gitleaksSHA256 are the release archives' checksums
// (gitleaks_<version>_checksums.txt), by GOOS_GOARCH.
var gitleaksSHA256 = map[string]string{
	"linux_amd64":  "551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb",
	"linux_arm64":  "e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080",
	"darwin_amd64": "dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709",
	"darwin_arm64": "b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5",
}

// gitleaks is the pinned binary, downloaded once into the user's cache.
func gitleaks(ctx context.Context) (string, error) {
	platform := runtime.GOOS + "_" + runtime.GOARCH
	want, known := gitleaksSHA256[platform]
	if !known {
		return "", fmt.Errorf("no pinned gitleaks for %s", platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("gitleaks: %w", err)
	}
	dir := filepath.Join(cache, "cilocal", "gitleaks-"+gitleaksVersion)
	binary := filepath.Join(dir, "gitleaks")
	if _, err := os.Stat(binary); err == nil {
		return binary, nil
	}
	asset := strings.Replace(platform, "amd64", "x64", 1)
	url := fmt.Sprintf("https://github.com/gitleaks/gitleaks/releases/download/v%s/gitleaks_%s_%s.tar.gz", gitleaksVersion, gitleaksVersion, asset)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("gitleaks: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("downloading gitleaks: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: %s", url, response.Status)
	}
	archive, err := io.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("downloading gitleaks: %w", err)
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return "", fmt.Errorf("%s has sha256 %s, expected %s", url, hex.EncodeToString(sum[:]), want)
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return "", fmt.Errorf("gitleaks: %w", err)
	}
	if err := untarOne(archive, "gitleaks", binary); err != nil {
		return "", err
	}
	return binary, nil
}

func untarOne(archive []byte, name, target string) error {
	unzipped, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return fmt.Errorf("gitleaks archive: %w", err)
	}
	reader := tar.NewReader(unzipped)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("gitleaks archive has no %s", name)
		}
		if err != nil {
			return fmt.Errorf("gitleaks archive: %w", err)
		}
		if header.Name != name {
			continue
		}
		partial := target + ".partial"
		file, err := os.OpenFile(partial, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, binaryMode)
		if err != nil {
			return fmt.Errorf("gitleaks: %w", err)
		}
		const maxBinary = 1 << 28 // 256 MiB, far above gitleaks' size
		if _, err := io.CopyN(file, reader, maxBinary); err != nil && !errors.Is(err, io.EOF) {
			_ = file.Close()
			return fmt.Errorf("gitleaks: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("gitleaks: %w", err)
		}
		if err := os.Rename(partial, target); err != nil {
			return fmt.Errorf("gitleaks: %w", err)
		}
		return nil
	}
}

// scanFiles is which files the arguments name (nil for every tracked file),
// the commit to read them from ("" for the checkout), and the base commit
// ("" without --base).
func scanFiles(ctx context.Context, args []string) ([]string, string, string, error) {
	flags := flag.NewFlagSet("secret-scan", flag.ContinueOnError)
	base := flags.String("base", "", "scan the files changed since this commit")
	pull := flags.String("pr", "", "scan this pull request's files (GitHub's list)")
	all := flags.Bool("all", false, "scan every tracked file")
	head := flags.String("head", "", "read the files from this commit's objects, not the checkout")
	if err := flags.Parse(args); err != nil {
		return nil, "", "", fmt.Errorf("secret-scan: %w", err)
	}
	files, err := namedFiles(ctx, flags, *base, *pull, *all)
	return files, *head, *base, err
}

func namedFiles(ctx context.Context, flags *flag.FlagSet, base, pull string, all bool) ([]string, error) {
	switch {
	case all:
		return nil, nil
	case base != "":
		out, err := git(ctx, "", "diff", "--name-only", "--no-renames", "--diff-filter=d", base, "HEAD")
		if err != nil {
			return nil, err
		}
		return strings.Fields(out), nil
	case pull != "":
		hub, err := newGitHub(ctx)
		if err != nil {
			return nil, err
		}
		out, err := output(ctx, "", "gh", "api", "--paginate", "repos/"+hub.repo+"/pulls/"+pull+"/files?per_page=100",
			"-q", `.[] | select(.status != "removed") | .filename`)
		files := strings.Split(out, "\n")
		if err != nil || len(files) >= gitHubFileLimit {
			fmt.Println("Scanning every file: could not list the pull request's files")
			return nil, nil //nolint:nilerr // fall back to scanning everything
		}
		return files, nil
	}
	return flags.Args(), nil
}

// gitHubFileLimit is the most files GitHub lists for a pull request.
const gitHubFileLimit = 3000

func secretScan(ctx context.Context, args []string) error {
	files, head, base, err := scanFiles(ctx, args)
	if err != nil {
		return err
	}
	// The policy is the trusted commit's: --base, or the checkout's HEAD (the
	// base, in the Secret scan job). The change compared with it is the head
	// read as data, or HEAD against --base; --all and named files scan none.
	trusted, scanned := "HEAD", head
	if base != "" {
		trusted = base
		if scanned == "" {
			scanned = "HEAD"
		}
	}
	changed, err := policyChanges(ctx, trusted, scanned)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "secret-scan-")
	if err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	// The trusted policy and the copies live side by side, never one inside
	// the other: gitleaks loads policy from the root of what it scans.
	policy, scan := filepath.Join(work, "policy"), filepath.Join(work, "scan")
	for _, dir := range []string{policy, scan} {
		if err := os.Mkdir(dir, dirMode); err != nil {
			return fmt.Errorf("secret-scan: %w", err)
		}
	}
	if err := writePolicy(ctx, trusted, policy); err != nil {
		return err
	}
	copyFiles := copyCheckout
	if head != "" {
		copyFiles = func(ctx context.Context, files []string, scan string) (int, error) {
			return copyCommit(ctx, head, files, scan)
		}
	}
	copied, err := copyFiles(ctx, files, scan)
	if err != nil {
		return err
	}
	if copied == 0 {
		fmt.Println("No changed files to scan.")
	} else if err := runGitleaks(ctx, scan, policy, copied); err != nil {
		return err
	}
	if problem := verify.PolicyProblem(changed); problem != "" {
		return errors.New(problem)
	}
	return nil
}

// policyChanges is the scanner policy files that differ between the trusted
// commit and the scanned one (verify.PolicyChanges); none when `scanned` is "".
func policyChanges(ctx context.Context, trusted, scanned string) ([]string, error) {
	if scanned == "" {
		return nil, nil
	}
	if err := fetchCommit(ctx, scanned); err != nil {
		return nil, err
	}
	trees := [2][]verify.TreeEntry{}
	for index, rev := range []string{trusted, scanned} {
		listing, err := git(ctx, "", "ls-tree", "-z", "--full-tree", rev)
		if err != nil {
			return nil, err
		}
		trees[index] = verify.ParseTree(listing)
	}
	return verify.PolicyChanges(trees[0], trees[1]), nil
}

// trustedDefault is the config when the trusted commit has no
// .gitleaks.toml: gitleaks' own rules, nothing allowed.
const trustedDefault = "[extend]\nuseDefault = true\n"

// writePolicy writes the trusted commit's .gitleaks.toml and .gitleaksignore
// into `dir` (gitleaks' defaults and an empty ignore list where it has none).
func writePolicy(ctx context.Context, rev, dir string) error {
	defaults := map[string]string{".gitleaks.toml": trustedDefault, ".gitleaksignore": ""}
	for _, name := range verify.ScannerPolicy {
		content := defaults[name]
		if _, err := git(ctx, "", "cat-file", "-e", rev+":"+name); err == nil {
			command := exec.CommandContext(ctx, "git", "cat-file", "blob", rev+":"+name)
			out, err := command.Output()
			if err != nil {
				return fmt.Errorf("git cat-file %s:%s: %w", rev, name, err)
			}
			content = string(out)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), fileMode); err != nil {
			return fmt.Errorf("secret-scan: %w", err)
		}
	}
	return nil
}

// fetchCommit fetches `rev` when the checkout lacks it: a pull request's
// head, a fork's included, is reachable from the base repository.
func fetchCommit(ctx context.Context, rev string) error {
	if _, err := git(ctx, "", "cat-file", "-e", rev+"^{commit}"); err == nil {
		return nil
	}
	_, err := git(ctx, "", "fetch", "--quiet", "--no-tags", "--depth=1", "origin", rev)
	return err
}

// copyCheckout copies the named files (every tracked one when nil) from the
// checkout into `scan`.
func copyCheckout(ctx context.Context, files []string, scan string) (int, error) {
	if files == nil {
		out, err := git(ctx, "", "ls-files")
		if err != nil {
			return 0, err
		}
		files = strings.Split(out, "\n")
	}
	root, err := git(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return 0, err
	}
	copied := 0
	for _, file := range files {
		if file == "" {
			continue
		}
		// Symlinks and submodules are not content of their own.
		info, err := os.Lstat(filepath.Join(root, file))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := copyFile(filepath.Join(root, file), filepath.Join(scan, verify.ScanCopyPath(file))); err != nil {
			return 0, err
		}
		copied++
	}
	return copied, nil
}

// copyCommit writes the named files (every one when nil) of commit `head`'s
// tree into `scan`, from git's objects: the commit is fetched if the
// checkout lacks it (a pull request's head, a fork's included, is reachable
// from the base repository), and nothing of it is checked out.
func copyCommit(ctx context.Context, head string, files []string, scan string) (int, error) {
	if err := fetchCommit(ctx, head); err != nil {
		return 0, err
	}
	listing, err := git(ctx, "", "ls-tree", "-r", "-z", "--full-tree", head)
	if err != nil {
		return 0, err
	}
	targets, problem := verify.ScanTargets(verify.ParseTree(listing), files)
	if problem != "" {
		return 0, errors.New("secret-scan: " + problem)
	}
	if len(targets) == 0 {
		return 0, nil
	}
	var request strings.Builder
	for _, target := range targets {
		request.WriteString(target.Blob + "\n")
	}
	command := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	command.Stdin = strings.NewReader(request.String())
	out, err := command.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("secret-scan: %w", err)
	}
	if err := command.Start(); err != nil {
		return 0, fmt.Errorf("secret-scan: %w", err)
	}
	reader := bufio.NewReader(out)
	for _, target := range targets {
		if err := writeBlob(reader, target, filepath.Join(scan, filepath.FromSlash(verify.ScanCopyPath(target.Path)))); err != nil {
			_ = command.Wait()
			return 0, err
		}
	}
	if err := command.Wait(); err != nil {
		return 0, fmt.Errorf("git cat-file: %w", err)
	}
	return len(targets), nil
}

// writeBlob reads one `git cat-file --batch` answer (`<sha> blob <size>`,
// the content, a newline) into `to`.
func writeBlob(reader *bufio.Reader, target verify.TreeEntry, to string) error {
	header, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("git cat-file: %w", err)
	}
	fields := strings.Fields(header)
	const headerFields = 3 // sha, type, size
	if len(fields) != headerFields || fields[0] != target.Blob || fields[1] != "blob" {
		return fmt.Errorf("git cat-file: %q for %s", strings.TrimSpace(header), target.Path)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return fmt.Errorf("git cat-file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(to), dirMode); err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	file, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	if _, err := io.CopyN(file, reader, size); err != nil {
		_ = file.Close()
		return fmt.Errorf("git cat-file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	if _, err := reader.Discard(1); err != nil { // the newline after the content
		return fmt.Errorf("git cat-file: %w", err)
	}
	return nil
}

// runGitleaks scans the copies in `scan` with the pinned gitleaks and the
// trusted policy in `policy` (verify.GitleaksArgs), with no GITLEAKS_* env,
// which would name another config.
func runGitleaks(ctx context.Context, scan, policy string, copied int) error {
	binary, err := gitleaks(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Scanning %d file(s) with gitleaks %s.\n", copied, gitleaksVersion)
	command := exec.CommandContext(ctx, binary, verify.GitleaksArgs(filepath.Join(policy, ".gitleaks.toml"), filepath.Join(policy, ".gitleaksignore"))...)
	command.Dir = scan
	command.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool { return strings.HasPrefix(variable, "GITLEAKS_") })
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("the secret scan found credential-shaped values (above, redacted): remove a real secret and rotate it; a deliberate fixture gets an obviously fake value and an exact allowlist entry in .gitleaks.toml (docs/CI.md, \"Secret scan\"): %w", err)
	}
	return nil
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), dirMode); err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	content, err := os.ReadFile(from)
	if err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	if err := os.WriteFile(to, content, fileMode); err != nil {
		return fmt.Errorf("secret-scan: %w", err)
	}
	return nil
}
