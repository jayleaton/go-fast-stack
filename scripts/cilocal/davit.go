package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// Davit boxes (optional; docs/CI.md, "Davit boxes"): when another full run
// holds this machine's slot, when the bash on PATH is too old for the repo's
// scripts, or with --davit, scripts/affected.sh and the steps run in a fresh
// Davit box of their own. The selection, the head checks, reuse, the secret
// scan and the migration order (which read git history and origin's refs)
// and posting stay on this machine. Davit syncs the clean checkout of the
// head into the box (files git does not ignore, with an index), `cilocal
// exec` there refuses to run anything unless the box's files are exactly
// the head's tree, the steps are the same, the report names
// `davit:<project>/<box>`, and the box is removed afterwards.

// boxWait is how long a run waits in the Davit host's queue for capacity.
const boxWait = "15m"

// boxMemory is the box's memory (CI_LOCAL_DAVIT_MEMORY overrides it): a full
// cold make test peaks around 2 GB beside lint and the release smoke.
const boxMemory = "4GiB"

// davitBox is the run's box: `name` is <project>/cilocal-<sha>, `dir` the
// checkout it syncs from.
type davitBox struct {
	name, dir string
}

// davitProject is the Davit project of this repository: its origin's name.
func davitProject(ctx context.Context) string {
	origin, err := git(ctx, "", "remote", "get-url", "origin")
	if err != nil {
		return "cilocal"
	}
	return strings.TrimSuffix(path.Base(strings.TrimSuffix(origin, "/")), ".git")
}

func newBox(ctx context.Context, work checkout, found state) *davitBox {
	return &davitBox{name: davitProject(ctx) + "/cilocal-" + verify.Short(found.head.SHA), dir: work.dir}
}

func (box *davitBox) remove(ctx context.Context) {
	remove := exec.CommandContext(ctx, "davit", "box", "rm", box.name, "--yes")
	remove.Dir = box.dir
	if out, err := remove.CombinedOutput(); err != nil {
		logf("could not remove the box %s: %v: %s", box.name, err, strings.TrimSpace(string(out)))
	}
}

// run syncs the checkout into the box and runs `cilocal exec` there with
// the payload, reading its markers.
func (box *davitBox) run(ctx context.Context, work payload, module string) (results, error) {
	empty := results{done: map[string]stepDone{}, tree: "", after: "", affected: "", failures: map[string]*verify.Failures{}}
	encoded, err := json.Marshal(work)
	if err != nil {
		return empty, fmt.Errorf("davit payload: %w", err)
	}
	memory := os.Getenv("CI_LOCAL_DAVIT_MEMORY")
	if memory == "" {
		memory = boxMemory
	}
	command := exec.CommandContext(ctx, "davit", "box", "run", box.name, "--wait", boxWait, "--memory", memory,
		"-e", payloadEnv+"="+base64.StdEncoding.EncodeToString(encoded), "--", "go", "run", "./scripts/cilocal", "exec")
	command.Dir = box.dir
	command.Stderr = os.Stderr
	reader, err := command.StdoutPipe()
	if err != nil {
		return empty, fmt.Errorf("davit: %w", err)
	}
	if err := command.Start(); err != nil {
		return empty, fmt.Errorf("davit: %w", err)
	}
	found := readResults(reader, os.Stderr, module)
	if err := command.Wait(); err != nil {
		return found, fmt.Errorf("davit box run %s: %w", box.name, err)
	}
	return found, nil
}

// affected runs scripts/affected.sh in the box: a pure function of the tree
// and the changed files, which the box gets as AFFECTED_FILES.
func (box *davitBox) affected(ctx context.Context, tree string, env map[string]string) (string, error) {
	logf("asking the Davit box %s which packages the change affects", box.name)
	found, err := box.run(ctx, payload{Tree: tree, Steps: nil, Env: env, KeepGoing: false, Affected: true}, "")
	if err != nil {
		return "", err
	}
	if found.tree != tree {
		return "", fmt.Errorf("the box's tree %s is not the head's %s", verify.Short(found.tree), verify.Short(tree))
	}
	if found.affected == "" {
		return "", errors.New("the box printed no affected.sh result")
	}
	return found.affected, nil
}
