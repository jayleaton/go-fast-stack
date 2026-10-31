package verify_test

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

var (
	goStep      = verify.Step{Item: "build", Job: verify.JobGo, Name: "go build", Run: []string{"go", "build", "./..."}}
	mobileStep  = verify.Step{Item: "mobile", Job: verify.JobMobile, Name: "mobile checks", Run: []string{"bash", "-c", "pnpm test"}}
	secretsStep = verify.Step{Item: "secrets", Job: verify.JobSecrets, Name: "gitleaks", Run: []string{"gitleaks"}, Local: true}
	orderStep   = verify.Step{Item: "migrations", Job: verify.JobMigrations, Name: "migration order", Run: []string{"./order.sh"}, Local: true}
)

func baseTree() []verify.TreeEntry {
	return []verify.TreeEntry{
		{Mode: "100644", Blob: "blob-main", Path: "apps/web/main.go"},
		{Mode: "100644", Blob: "blob-docs", Path: "docs/CI.md"},
		{Mode: "100644", Blob: "blob-readme", Path: "README.md"},
		{Mode: "100644", Blob: "blob-app", Path: "apps/mobile-app/app.ts"},
		{Mode: "100644", Blob: "blob-make", Path: "Makefile"},
	}
}

func withBlob(entries []verify.TreeEntry, file, blob string) []verify.TreeEntry {
	changed := slices.Clone(entries)
	for index := range changed {
		if changed[index].Path == file {
			changed[index].Blob = blob
		}
	}
	return changed
}

func TestInputHashesFollowTheStepsInputs(test *testing.T) {
	test.Parallel()
	const runner = "local darwin/arm64"
	steps := []verify.Step{goStep, mobileStep}
	base := verify.InputHashes(steps, baseTree(), runner)
	if len(base[goStep.ID()]) != 16 || len(base[mobileStep.ID()]) != 16 {
		test.Fatalf("hashes = %v, want 16 hex characters each", base)
	}
	changedArgs := goStep
	changedArgs.Run = []string{"go", "build", "./apps/..."}
	changedEnv := goStep
	changedEnv.Env = map[string]string{"CGO_ENABLED": "0"}
	for _, testCase := range []struct {
		name          string
		steps         []verify.Step
		entries       []verify.TreeEntry
		runner        string
		goChanges     bool
		mobileChanges bool
	}{
		{"same inputs", steps, baseTree(), runner, false, false},
		{"go blob", steps, withBlob(baseTree(), "apps/web/main.go", "blob-main-2"), runner, true, false},
		{"docs blob", steps, withBlob(baseTree(), "docs/CI.md", "blob-docs-2"), runner, false, false},
		{"root markdown blob", steps, withBlob(baseTree(), "README.md", "blob-readme-2"), runner, false, false},
		{"mobile blob", steps, withBlob(baseTree(), "apps/mobile-app/app.ts", "blob-app-2"), runner, false, true},
		{"makefile feeds go not mobile", steps, withBlob(baseTree(), "Makefile", "blob-make-2"), runner, true, false},
		{"new go file", steps, append(baseTree(), verify.TreeEntry{Mode: "100644", Blob: "blob-new", Path: "apps/web/new.go"}), runner, true, false},
		{"runner", steps, baseTree(), "davit linux/amd64", true, true},
		{"go args", []verify.Step{changedArgs, mobileStep}, baseTree(), runner, true, false},
		{"go env", []verify.Step{changedEnv, mobileStep}, baseTree(), runner, true, false},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			got := verify.InputHashes(testCase.steps, testCase.entries, testCase.runner)
			if changed := got[goStep.ID()] != base[goStep.ID()]; changed != testCase.goChanges {
				test.Fatalf("go hash changed = %v, want %v", changed, testCase.goChanges)
			}
			if changed := got[mobileStep.ID()] != base[mobileStep.ID()]; changed != testCase.mobileChanges {
				test.Fatalf("mobile hash changed = %v, want %v", changed, testCase.mobileChanges)
			}
		})
	}
}

func TestSecretsAndMigrationsAreNeverReused(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		step     verify.Step
		reusable bool
	}{
		{secretsStep, false},
		{orderStep, false},
		{goStep, true},
		{mobileStep, true},
		{verify.Step{Job: verify.JobDeploy, Name: "make deploy-check"}, true},
		{verify.Step{Job: verify.JobWorkflows, Name: "make actionlint"}, true},
	} {
		test.Run(testCase.step.ID(), func(test *testing.T) {
			test.Parallel()
			if got := verify.Reusable(testCase.step); got != testCase.reusable {
				test.Fatalf("Reusable = %v, want %v", got, testCase.reusable)
			}
			_, hashed := verify.InputHashes([]verify.Step{testCase.step}, baseTree(), "local")[testCase.step.ID()]
			if hashed != testCase.reusable {
				test.Fatalf("hashed = %v, want %v", hashed, testCase.reusable)
			}
		})
	}
}

func TestInputOfMatchesSelection(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		job, file string
		want      bool
	}{
		{verify.JobGo, "apps/web/x.go", true},
		{verify.JobGo, "docs/x.md", false},
		{verify.JobDeploy, "deploy/main.go", true},
		{verify.JobDeploy, "apps/web/x.go", false},
		{verify.JobMobile, "apps/web/internal/config/routes.go", true},
		{verify.JobMobile, "Makefile", false},
		{verify.JobWorkflows, ".github/workflows/ci.yml", true},
		{verify.JobWorkflows, "apps/web/x.go", false},
		{verify.JobSecrets, "docs/x.md", true},
	} {
		test.Run(testCase.job+" "+testCase.file, func(test *testing.T) {
			test.Parallel()
			if got := verify.InputOf(testCase.job, testCase.file); got != testCase.want {
				test.Fatalf("InputOf = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestParseTreeAndTreeDigest(test *testing.T) {
	test.Parallel()
	output := "100644 blob aaa\tapps/web/main.go\x00100755 blob bbb\tscripts/run.sh\x00garbage\x00040000 tree\tbad\x00"
	entries := verify.ParseTree(output)
	want := []verify.TreeEntry{
		{Mode: "100644", Blob: "aaa", Path: "apps/web/main.go"},
		{Mode: "100755", Blob: "bbb", Path: "scripts/run.sh"},
	}
	if !slices.Equal(entries, want) {
		test.Fatalf("ParseTree = %+v, want %+v", entries, want)
	}
	reversed := slices.Clone(entries)
	slices.Reverse(reversed)
	if verify.TreeDigest(reversed) != verify.TreeDigest(entries) {
		test.Fatal("TreeDigest depends on entry order")
	}
	if verify.TreeDigest(withBlob(entries, "scripts/run.sh", "ccc")) == verify.TreeDigest(entries) {
		test.Fatal("TreeDigest ignores a blob change")
	}
	if !slices.Equal(entries, want) {
		test.Fatal("TreeDigest reordered its input")
	}
}

func TestLedgerStepJSON(test *testing.T) {
	test.Parallel()
	for _, testCase := range []struct {
		name string
		step verify.LedgerStep
		json string
	}{
		{"own", verify.LedgerStep{ID: "go › go build", Hash: "h1"}, `["go › go build","h1"]`},
		{"reused", verify.LedgerStep{ID: "go › go build", Hash: "h1", From: oldSHA}, `["go › go build","h1","` + oldSHA + `"]`},
	} {
		test.Run(testCase.name, func(test *testing.T) {
			test.Parallel()
			encoded, err := json.Marshal(testCase.step)
			if err != nil || string(encoded) != testCase.json {
				test.Fatalf("Marshal = %s, %v; want %s", encoded, err, testCase.json)
			}
			var decoded verify.LedgerStep
			if err := json.Unmarshal(encoded, &decoded); err != nil || decoded != testCase.step {
				test.Fatalf("Unmarshal = %+v, %v; want %+v", decoded, err, testCase.step)
			}
		})
	}
	for _, bad := range []string{`["only"]`, `["a","b","c","d"]`, `{"id":"x"}`} {
		test.Run("rejects "+bad, func(test *testing.T) {
			test.Parallel()
			var decoded verify.LedgerStep
			if err := json.Unmarshal([]byte(bad), &decoded); err == nil {
				test.Fatalf("Unmarshal(%s) accepted %+v", bad, decoded)
			}
		})
	}
}

func attested(ledger verify.Ledger) []verify.Status {
	return []verify.Status{success("passed in 1m02s · body 000000000000 · ledger " + verify.LedgerDigest(ledger) + " · on laptop")}
}

func TestLendersKeepOnlyAttestedEarlierHeads(test *testing.T) {
	test.Parallel()
	step := verify.LedgerStep{ID: goStep.ID(), Hash: "h1"}
	good := verify.Ledger{Head: oldSHA, Steps: []verify.LedgerStep{step}}
	failed := verify.Ledger{Head: strings.Repeat("1", 40), Steps: []verify.LedgerStep{step}}
	unattested := verify.Ledger{Head: strings.Repeat("2", 40), Steps: []verify.LedgerStep{step}}
	otherDigest := verify.Ledger{Head: strings.Repeat("3", 40), Steps: []verify.LedgerStep{step}}
	errored := verify.Ledger{Head: strings.Repeat("4", 40), Steps: []verify.LedgerStep{step}}
	pending := verify.Ledger{Head: strings.Repeat("5", 40), Steps: []verify.LedgerStep{step}}
	self := verify.Ledger{Head: headSHA, Steps: []verify.LedgerStep{step}}
	statuses := map[string][]verify.Status{
		good.Head: append([]verify.Status{{Context: "other", State: "failure"}}, attested(good)...),
		failed.Head: append([]verify.Status{{Context: verify.StatusContext, State: "failure", CreatedAt: "then"}},
			attested(failed)...),
		unattested.Head:  {success("passed with no digest")},
		otherDigest.Head: attested(verify.Ledger{Head: otherDigest.Head, Steps: []verify.LedgerStep{{ID: "x", Hash: "y"}}}),
		errored.Head:     append(attested(errored), verify.Status{Context: verify.StatusContext, State: "error"}),
		pending.Head:     {{Context: verify.StatusContext, State: "pending", Description: "ledger " + verify.LedgerDigest(pending)}},
		self.Head:        attested(self),
	}
	calls := map[string]int{}
	lenders := verify.Lenders([]verify.Ledger{self, good, failed, good, unattested, otherDigest, errored, pending}, headSHA, func(sha string) []verify.Status {
		calls[sha]++
		return statuses[sha]
	})
	if len(lenders) != 1 || lenders[0].Head != good.Head {
		test.Fatalf("Lenders = %+v, want only %s", lenders, good.Head)
	}
	if calls[headSHA] != 0 || calls[good.Head] != 1 {
		test.Fatalf("status lookups = %v, want none for the head and one per earlier head", calls)
	}
}

func TestReusesMapsStepsToTheirLender(test *testing.T) {
	test.Parallel()
	first := verify.Ledger{Head: oldSHA, Steps: []verify.LedgerStep{{ID: "go › go build", Hash: "h1"}, {ID: "go › make test", Hash: "h2"}}}
	second := verify.Ledger{Head: otherSHA, Steps: []verify.LedgerStep{{ID: "go › go build", Hash: "h1"}, {ID: "deploy › make deploy-check", Hash: "h3"}}}
	got := verify.Reuses(map[string]string{
		"go › go build":              "h1",
		"go › make test":             "changed",
		"deploy › make deploy-check": "h3",
		"mobile › mobile checks":     "h4",
	}, []verify.Ledger{first, second})
	want := map[string]verify.Lent{
		"go › go build":              {From: oldSHA, Hash: "h1"},
		"deploy › make deploy-check": {From: otherSHA, Hash: "h3"},
	}
	if !maps.Equal(got, want) {
		test.Fatalf("Reuses = %+v, want %+v", got, want)
	}
	if empty := verify.Reuses(map[string]string{"go › go build": "h1"}, nil); len(empty) != 0 {
		test.Fatalf("Reuses with no lenders = %+v", empty)
	}
}
