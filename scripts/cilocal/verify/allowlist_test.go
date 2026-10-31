package verify_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayleaton/go-fast-stack/scripts/cilocal/verify"
)

// A path in an allowlist may only narrow a value exception: exemption by
// path alone would let any credential in a test, testdata, an example env
// file or the bench script pass the scan. Randy's re-review of #134: the same
// synthetic token passed in all five of those locations.
func TestAllowlistProblem(test *testing.T) {
	test.Parallel()
	cases := map[string]struct {
		config string
		want   string
	}{
		"path only": {
			config: "[extend]\nuseDefault = true\n[allowlist]\npaths = [\n  '''_test\\.go$''',\n  '''(^|/)testdata/''',\n]\n",
			want:   "`paths` without `regexes` or `stopwords`",
		},
		"path and values are ORed without condition": {
			config: "[extend]\nuseDefault = true\n[allowlist]\npaths = ['''^scripts/bench-devloop\\.sh$''']\nregexes = ['''^bench-secret$''']\n",
			want:   "not `condition = \"AND\"`",
		},
		"path only in a rule allowlist": {
			config: "[extend]\nuseDefault = true\n[[rules]]\nid = \"generic-api-key\"\n[[rules.allowlists]]\ncondition = \"AND\"\npaths = ['''\\.env\\.example$''']\n",
			want:   "`paths` without `regexes` or `stopwords`",
		},
		"path narrowed by a value and AND": {
			config: "[extend]\nuseDefault = true\n[[rules]]\nid = \"generic-api-key\"\n[[rules.allowlists]]\ncondition = \"AND\"\npaths = ['''^scripts/bench-devloop\\.sh$''']\nregexes = ['''^bench-secret$''']\n",
		},
		"path narrowed by stopwords and AND": {
			config: "[extend]\nuseDefault = true\n[allowlist]\ncondition = \"AND\"\npaths = ['''(^|/)docs/''']\nstopwords = ['example']\n",
		},
		"value only": {
			config: "[extend]\nuseDefault = true\n[allowlist]\nregexes = ['''^bench-secret$''']\n",
		},
		"empty paths": {
			config: "[extend]\nuseDefault = true\n[allowlist]\npaths = []\n",
		},
		"rule table after an allowlist ends it": {
			config: "[extend]\nuseDefault = true\n[allowlist]\nregexes = ['''^bench-secret$''']\n[[rules]]\nid = \"generic-api-key\"\n",
		},
	}
	for name, testCase := range cases {
		problem := verify.AllowlistProblem(testCase.config)
		if testCase.want == "" {
			if problem != "" {
				test.Errorf("%s: AllowlistProblem = %q, want none", name, problem)
			}
			continue
		}
		if !strings.Contains(problem, testCase.want) {
			test.Errorf("%s: AllowlistProblem = %q, want it to name %q", name, problem, testCase.want)
		}
	}
}

// The repository's own policy keeps that rule: the scan's allowlist names
// synthetic values, never a path on its own. A blanket path allowlist fails
// here before it can make a real credential invisible.
func TestScannerPolicyAllowlistsValuesOnly(test *testing.T) {
	test.Parallel()
	config, err := os.ReadFile(filepath.Join("..", "..", "..", ".gitleaks.toml"))
	if err != nil {
		test.Fatal(err)
	}
	if problem := verify.AllowlistProblem(string(config)); problem != "" {
		test.Fatalf(".gitleaks.toml allows a whole path instead of a fixture value: %s", problem)
	}
}
