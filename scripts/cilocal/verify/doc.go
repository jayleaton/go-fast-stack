// Package verify holds the rules of local verification (docs/CI.md, "Local
// verification"): the agent runs what its change needs, then `make ci-local
// RAN="<what you ran>"` attests it on the exact pushed head. This package
// renders the PR body section that attestation fills and decides, for the
// Local verification job in ci.yml, `make pr-merge` and the staging gate,
// whether a head is attested. It also holds the merge decision, the staging
// verdict after a merge, the migration order and the post-merge report's
// workflow list. It is pure: git, gh and HTTP run in scripts/cilocal, which
// passes their results in. Its tests are a test boundary (AGENTS.md "Test
// boundaries").
package verify
