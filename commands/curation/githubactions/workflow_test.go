package githubactions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseWorkflowUses(t *testing.T) {
	fixtureCI, err := os.ReadFile(filepath.Join(fixturesRoot, "curation-project", ".github", "workflows", "ci.yml"))
	require.NoError(t, err)
	twoJobs := "jobs:\n" + jobWithUses("build", "actions/checkout@v4") + jobWithUses("publish", "actions/upload-artifact@v4")

	tests := []struct {
		name     string
		workflow string // the workflow file's content
		jobID    string // the job to attribute against; "" is a local invocation, since a runner always sets GITHUB_JOB
		want     JobUses
		// wantErr is checked with errors.Is; nil means the parse succeeds.
		wantErr error
		// wantErrContains names what the message must surface so a mismatch is diagnosable from the log alone.
		wantErrContains []string
	}{
		{
			name:     "verify when the job declares remote, subpath, local and run steps then remote and local uses are returned in file order",
			workflow: string(fixtureCI),
			jobID:    "build",
			want: JobUses{
				Remote: []WorkflowUse{
					{Owner: "actions", Repo: "checkout", Ref: "v4", Raw: "actions/checkout@v4"},
					{Owner: "github", Repo: "codeql-action", Subpath: "analyze", Ref: "v3", Raw: "github/codeql-action/analyze@v3"},
				},
				// The job's own workflow declares it, so there is no declaring action to attribute it to.
				Local: []LocalUse{{Raw: "./.github/actions/build-prep"}},
			},
		},
		{
			name:     "verify when the file has two jobs and the first runs then only its own uses are returned",
			workflow: twoJobs,
			jobID:    "build",
			want:     JobUses{Remote: []WorkflowUse{{Owner: "actions", Repo: "checkout", Ref: "v4", Raw: "actions/checkout@v4"}}},
		},
		{
			name:     "verify when the file has two jobs and the second runs then only its own uses are returned",
			workflow: twoJobs,
			jobID:    "publish",
			want:     JobUses{Remote: []WorkflowUse{{Owner: "actions", Repo: "upload-artifact", Ref: "v4", Raw: "actions/upload-artifact@v4"}}},
		},
		{
			name:            "verify when the file does not declare the job then nothing is returned and the error names it and the jobs that exist",
			workflow:        twoJobs,
			jobID:           "a-job-declared-somewhere-else",
			wantErr:         ErrJobUnknown,
			wantErrContains: []string{"a-job-declared-somewhere-else", "build", "publish"},
		},
		{
			name:     "verify when no job id is given then nothing is returned and attribution is refused",
			workflow: twoJobs,
			jobID:    "",
			wantErr:  ErrJobUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(writeWorkflows(t, map[string]string{"ci.yml": tt.workflow}), "ci.yml")

			got, err := ParseWorkflowUses(path, tt.jobID)

			assert.ErrorIs(t, err, tt.wantErr, "ParseWorkflowUses(%q) error", tt.jobID)
			for _, want := range tt.wantErrContains {
				assert.ErrorContains(t, err, want)
			}
			assert.Equal(t, tt.want, got, "ParseWorkflowUses(%q)", tt.jobID)
		})
	}
}

func TestParseUsesString(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want WorkflowUse
		ok   bool
	}{
		{"verify when the reference is owner repo and ref then it parses", "actions/checkout@v4", WorkflowUse{Owner: "actions", Repo: "checkout", Ref: "v4", Raw: "actions/checkout@v4"}, true},
		{"verify when the reference carries a subpath then the subpath is captured", "github/codeql-action/analyze@v3", WorkflowUse{Owner: "github", Repo: "codeql-action", Subpath: "analyze", Ref: "v3", Raw: "github/codeql-action/analyze@v3"}, true},
		{"verify when another subpath of the same repo is used then it parses independently", "github/codeql-action/init@v3", WorkflowUse{Owner: "github", Repo: "codeql-action", Subpath: "init", Ref: "v3", Raw: "github/codeql-action/init@v3"}, true},
		{"verify when the subpath is nested then the whole remainder is captured", "a/b/c/d@v1", WorkflowUse{Owner: "a", Repo: "b", Subpath: "c/d", Ref: "v1", Raw: "a/b/c/d@v1"}, true},
		{"verify when the reference is a local action then it is skipped", "./.github/actions/build-prep", WorkflowUse{}, false},
		{"verify when the reference is a docker uri then it is skipped", "docker://alpine:3", WorkflowUse{}, false},
		{"verify when the reference has no ref then it is skipped", "actions/checkout", WorkflowUse{}, false},
		{"verify when the reference is empty then it is skipped", "", WorkflowUse{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseUsesString(tt.raw)
			assert.Equal(t, tt.ok, ok)
			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

// discoveredAction is one entry in a cross-reference case's action cache: the triple the walk
// would have found, plus the action.yml bodies to plant under it keyed by subpath ("" for the
// cache root). Expressing the fixture this way keeps the cases data rather than per-row setup.
type discoveredAction struct {
	key   string            // "owner/repo@ref"
	yamls map[string]string // subpath -> action.yml body
}

// compositeYAML is an action.yml for a composite action whose single step references uses.
func compositeYAML(uses string) string {
	return "runs:\n  using: composite\n  steps:\n    - uses: " + uses + "\n"
}

// compositeYAMLWithSteps is an action.yml for a composite action with several uses: steps, for
// mixing remote references and local ones in a single action.
func compositeYAMLWithSteps(uses ...string) string {
	yaml := "runs:\n  using: composite\n  steps:\n"
	for _, use := range uses {
		yaml += "    - uses: " + use + "\n"
	}
	return yaml
}

// nodeYAML is an action.yml for a non-composite action, which references nothing.
const nodeYAML = "runs:\n  using: node20\n"

// unreadableYAML is accepted by GitHub's runner but rejected by yaml.v3 (duplicate mapping keys).
const unreadableYAML = "name: w\nname: w\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-node@v4\n"

// buildDiscovered materializes each action into its own directory and returns the []ActionRef
// CrossReference would have been handed.
func buildDiscovered(t *testing.T, actions []discoveredAction) []ActionRef {
	t.Helper()
	refs := make([]ActionRef, 0, len(actions))
	for _, a := range actions {
		owner, rest, _ := strings.Cut(a.key, "/")
		repo, ref, _ := strings.Cut(rest, "@")
		dir := t.TempDir()
		for subpath, body := range a.yamls {
			target := filepath.Join(dir, filepath.FromSlash(subpath))
			require.NoError(t, os.MkdirAll(target, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(target, "action.yml"), []byte(body), 0600))
		}
		refs = append(refs, ActionRef{Owner: owner, Repo: repo, Ref: ref, Path: dir})
	}
	return refs
}

// attribution is what CrossReference adds to one cache entry.
type attribution struct {
	parent   string   // "" when used directly, or when no workflow or action explains the entry
	subpaths []string // the subpaths it was invoked through; nil when only its root was used
}

func remoteUse(owner, repo, ref, subpath string) WorkflowUse {
	return WorkflowUse{Owner: owner, Repo: repo, Ref: ref, Subpath: subpath}
}

func TestCrossReference(t *testing.T) {
	tests := []struct {
		name       string
		discovered []discoveredAction
		used       JobUses
		// want is the attribution of every entry returned, keyed "owner/repo@ref"; attribution is
		// additive, so every discovered entry must be here.
		want map[string]attribution
		// wantLocal is every local step the walk found, the job's own first.
		wantLocal []LocalUse
	}{
		{
			name:       "verify when an entry is used directly then it gets no parent and no unused subpath",
			discovered: []discoveredAction{{key: "actions/checkout@v4"}},
			used:       JobUses{Remote: []WorkflowUse{remoteUse("actions", "checkout", "v4", "")}},
			want:       map[string]attribution{"actions/checkout@v4": {}},
		},
		{
			name: "verify when an entry is used directly and by a composite then both are named",
			discovered: []discoveredAction{
				{key: "aquasecurity/setup-trivy@v1", yamls: map[string]string{"": compositeYAML("actions/cache/save@v4")}},
				{key: "actions/cache@v4"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("actions", "cache", "v4", "restore"), remoteUse("aquasecurity", "setup-trivy", "v1", "")}},
			want: map[string]attribution{
				"aquasecurity/setup-trivy@v1": {},
				"actions/cache@v4":            {parent: "direct; also via aquasecurity/setup-trivy@v1", subpaths: []string{"restore", "save"}},
			},
		},
		{
			name:       "verify when no workflow explains an entry then its parent stays empty rather than guessed",
			discovered: []discoveredAction{{key: "some-org/mystery-action@v1"}},
			want:       map[string]attribution{"some-org/mystery-action@v1": {}},
		},
		{
			name:       "verify when an entry cannot be attributed then it is still not dropped",
			discovered: []discoveredAction{{key: "actions/checkout@v4"}, {key: "some-other-org/unexplained@v9"}},
			used:       JobUses{Remote: []WorkflowUse{remoteUse("actions", "checkout", "v4", "")}},
			want:       map[string]attribution{"actions/checkout@v4": {}, "some-other-org/unexplained@v9": {}},
		},
		{
			// codeql-action is commonly invoked twice in one job, init@v3 then analyze@v3.
			name:       "verify when a monorepo action is invoked via several subpaths then all of them are collected in file order",
			discovered: []discoveredAction{{key: "github/codeql-action@v3"}},
			used:       JobUses{Remote: []WorkflowUse{remoteUse("github", "codeql-action", "v3", "init"), remoteUse("github", "codeql-action", "v3", "analyze")}},
			want:       map[string]attribution{"github/codeql-action@v3": {subpaths: []string{"init", "analyze"}}},
		},
		{
			// Subpaths are keyed on owner/repo@ref, so each ref carries only the subpath it was invoked through.
			name:       "verify when two refs of one monorepo are invoked through different subpaths then the subpaths do not bleed between them",
			discovered: []discoveredAction{{key: "github/codeql-action@v2"}, {key: "github/codeql-action@v3"}},
			used:       JobUses{Remote: []WorkflowUse{remoteUse("github", "codeql-action", "v2", "init"), remoteUse("github", "codeql-action", "v3", "analyze")}},
			want: map[string]attribution{
				"github/codeql-action@v2": {subpaths: []string{"init"}},
				"github/codeql-action@v3": {subpaths: []string{"analyze"}},
			},
		},
		{
			name: "verify when a transitive reference carries a subpath then the subpath survives",
			discovered: []discoveredAction{
				{key: "my-org/wrapper-action@v1", yamls: map[string]string{"": compositeYAML("github/codeql-action/analyze@v3")}},
				{key: "github/codeql-action@v3"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("my-org", "wrapper-action", "v1", "")}},
			want: map[string]attribution{
				"my-org/wrapper-action@v1": {},
				"github/codeql-action@v3":  {parent: "my-org/wrapper-action@v1", subpaths: []string{"analyze"}},
			},
		},
		{
			// The cache root is deliberately non-composite, so falling back to it would leave both
			// transitive entries unattributed.
			name: "verify when subpaths have their own metadata then each is read rather than the cache root",
			discovered: []discoveredAction{
				{key: "github/codeql-action@v3", yamls: map[string]string{
					"":        nodeYAML,
					"init":    compositeYAML("org/from-init@v1"),
					"analyze": compositeYAML("org/from-analyze@v1"),
				}},
				{key: "org/from-init@v1"},
				{key: "org/from-analyze@v1"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("github", "codeql-action", "v3", "init"), remoteUse("github", "codeql-action", "v3", "analyze")}},
			want: map[string]attribution{
				"github/codeql-action@v3": {subpaths: []string{"init", "analyze"}},
				"org/from-init@v1":        {parent: "github/codeql-action@v3"},
				"org/from-analyze@v1":     {parent: "github/codeql-action@v3"},
			},
		},
		{
			// Both locations carry their own action.yml with a different child, so both must be read -
			// not just the subpath, which is what collectSubpaths' root-drop bug left out.
			name: "verify when a monorepo is used at its root and through a subpath then both metadata locations are read",
			discovered: []discoveredAction{
				{key: "github/codeql-action@v3", yamls: map[string]string{
					"":     compositeYAML("org/from-root@v1"),
					"init": compositeYAML("org/from-init@v1"),
				}},
				{key: "org/from-root@v1"},
				{key: "org/from-init@v1"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("github", "codeql-action", "v3", ""), remoteUse("github", "codeql-action", "v3", "init")}},
			want: map[string]attribution{
				"github/codeql-action@v3": {subpaths: []string{"init"}},
				"org/from-root@v1":        {parent: "github/codeql-action@v3"},
				"org/from-init@v1":        {parent: "github/codeql-action@v3"},
			},
		},
		{
			// The first parent in file order wins Parent, but the child's subpath metadata, pulled in only
			// through the second parent, must still be scanned once the child is attributed.
			name: "verify when a child is referenced by two parents at different subpaths then both are scanned",
			discovered: []discoveredAction{
				{key: "org/parent-a@v1", yamls: map[string]string{"": compositeYAML("org/shared-child@v1")}},
				{key: "org/parent-b@v1", yamls: map[string]string{"": compositeYAML("org/shared-child/sub@v1")}},
				{key: "org/shared-child@v1", yamls: map[string]string{"sub": compositeYAML("org/from-sub@v1")}},
				{key: "org/from-sub@v1"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("org", "parent-a", "v1", ""), remoteUse("org", "parent-b", "v1", "")}},
			want: map[string]attribution{
				"org/parent-a@v1":     {},
				"org/parent-b@v1":     {},
				"org/shared-child@v1": {parent: "org/parent-a@v1", subpaths: []string{"sub"}},
				"org/from-sub@v1":     {parent: "org/shared-child@v1"},
			},
		},
		{
			name: "verify when a composite action.yml cannot be read then its child survives unattributed and no local step is invented",
			discovered: []discoveredAction{
				{key: "some-org/wrapper@v1", yamls: map[string]string{"": unreadableYAML}},
				{key: "actions/setup-node@v4"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("some-org", "wrapper", "v1", "")}},
			want: map[string]attribution{"some-org/wrapper@v1": {}, "actions/setup-node@v4": {}},
		},
		{
			name: "verify when nothing was discovered then nothing is returned",
		},
		{
			// There is no fixed depth constant: the walk's bound scales with the cache.
			name: "verify when a chain is six actions long then every hop is attributed",
			discovered: []discoveredAction{
				{key: "org/action1@v1", yamls: map[string]string{"": compositeYAML("org/action2@v1")}},
				{key: "org/action2@v1", yamls: map[string]string{"": compositeYAML("org/action3@v1")}},
				{key: "org/action3@v1", yamls: map[string]string{"": compositeYAML("org/action4@v1")}},
				{key: "org/action4@v1", yamls: map[string]string{"": compositeYAML("org/action5@v1")}},
				{key: "org/action5@v1", yamls: map[string]string{"": compositeYAML("org/action6@v1")}},
				{key: "org/action6@v1", yamls: map[string]string{"": nodeYAML}},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("org", "action1", "v1", "")}},
			want: map[string]attribution{
				"org/action1@v1": {},
				"org/action2@v1": {parent: "org/action1@v1"},
				"org/action3@v1": {parent: "org/action2@v1"},
				"org/action4@v1": {parent: "org/action3@v1"},
				"org/action5@v1": {parent: "org/action4@v1"},
				"org/action6@v1": {parent: "org/action5@v1"},
			},
		},
		{
			// Two cache entries, but the chain to the second runs through three subpath locations, so
			// attribution needs more rounds than there are entries.
			name: "verify when a chain runs through more subpaths than the cache has entries then every hop is attributed",
			discovered: []discoveredAction{
				{key: "org/mono@v1", yamls: map[string]string{
					"":   compositeYAML("org/mono/s1@v1"),
					"s1": compositeYAML("org/mono/s2@v1"),
					"s2": compositeYAML("org/mono/s3@v1"),
					"s3": compositeYAML("org/leaf@v1"),
				}},
				{key: "org/leaf@v1"},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("org", "mono", "v1", "")}},
			want: map[string]attribution{
				"org/mono@v1": {parent: "direct; also via org/mono@v1", subpaths: []string{"s1", "s2", "s3"}}, // Known bug: the action is named as its own parent; the fix makes this parent "".
				"org/leaf@v1": {parent: "org/mono@v1"},
			},
		},
		{
			// A subpath listed in the report must have had its metadata read, or the report overstates coverage.
			name: "verify when one entry chains through two of its own subpaths then the last one's metadata is read",
			discovered: []discoveredAction{{key: "org/mono@v1", yamls: map[string]string{
				"":   compositeYAML("org/mono/s1@v1"),
				"s1": compositeYAML("org/mono/s2@v1"),
				"s2": compositeYAML("./local-at-s2"),
			}}},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("org", "mono", "v1", "")}},
			want:      map[string]attribution{"org/mono@v1": {parent: "direct; also via org/mono@v1", subpaths: []string{"s1", "s2"}}}, // Known bug: the action is named as its own parent; the fix makes this parent "".
			wantLocal: []LocalUse{{Raw: "./local-at-s2", DeclaredBy: "org/mono@v1"}},
		},
		{
			name: "verify when one entry chains through four of its own subpaths then the walk still reaches the end",
			discovered: []discoveredAction{{key: "org/mono@v1", yamls: map[string]string{
				"":   compositeYAML("org/mono/s1@v1"),
				"s1": compositeYAML("org/mono/s2@v1"),
				"s2": compositeYAML("org/mono/s3@v1"),
				"s3": compositeYAML("org/mono/s4@v1"),
				"s4": compositeYAML("./local-at-s4"),
			}}},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("org", "mono", "v1", "")}},
			want:      map[string]attribution{"org/mono@v1": {parent: "direct; also via org/mono@v1", subpaths: []string{"s1", "s2", "s3", "s4"}}}, // Known bug: the action is named as its own parent; the fix makes this parent "".
			wantLocal: []LocalUse{{Raw: "./local-at-s4", DeclaredBy: "org/mono@v1"}},
		},
		{
			// A hang fails the run at go test's timeout; the cycle must not overwrite the direct entry.
			name: "verify when two actions reference each other then the walk ends and the direct one keeps no parent",
			discovered: []discoveredAction{
				{key: "org/action1@v1", yamls: map[string]string{"": compositeYAML("org/action2@v1")}},
				{key: "org/action2@v1", yamls: map[string]string{"": compositeYAML("org/action1@v1")}},
			},
			used: JobUses{Remote: []WorkflowUse{remoteUse("org", "action1", "v1", "")}},
			want: map[string]attribution{"org/action1@v1": {}, "org/action2@v1": {parent: "org/action1@v1"}},
		},
		{
			// Every hop is a new location on a key that is already attributed, so only the location dedup stops it.
			name: "verify when subpaths of one entry reference each other then the walk ends and each location is scanned once",
			discovered: []discoveredAction{{key: "org/mono@v1", yamls: map[string]string{
				"":   compositeYAML("org/mono/s1@v1"),
				"s1": compositeYAML("org/mono/s2@v1"),
				"s2": compositeYAML("org/mono/s1@v1"),
			}}},
			used: JobUses{Remote: []WorkflowUse{remoteUse("org", "mono", "v1", "")}},
			want: map[string]attribution{"org/mono@v1": {parent: "direct; also via org/mono@v1", subpaths: []string{"s1", "s2"}}}, // Known bug: the action is named as its own parent; the fix makes this parent "".
		},
		{
			// A relative uses: inside a composite names a path in the caller's repository, resolved from the
			// workspace rather than from the cache this command read.
			name:       "verify when a composite action declares a local step then it is reported against that action",
			discovered: []discoveredAction{{key: "some-org/wrapper@v1", yamls: map[string]string{"": compositeYAML("./scripts/build")}}},
			used:       JobUses{Remote: []WorkflowUse{remoteUse("some-org", "wrapper", "v1", "")}},
			want:       map[string]attribution{"some-org/wrapper@v1": {}},
			wantLocal:  []LocalUse{{Raw: "./scripts/build", DeclaredBy: "some-org/wrapper@v1"}},
		},
		{
			name: "verify when the local step sits several hops out then the walk still reaches it",
			discovered: []discoveredAction{
				{key: "org/outer@v1", yamls: map[string]string{"": compositeYAML("org/inner@v1")}},
				{key: "org/inner@v1", yamls: map[string]string{"": compositeYAML("./deep/local")}},
			},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("org", "outer", "v1", "")}},
			want:      map[string]attribution{"org/outer@v1": {}, "org/inner@v1": {parent: "org/outer@v1"}},
			wantLocal: []LocalUse{{Raw: "./deep/local", DeclaredBy: "org/inner@v1"}},
		},
		{
			name: "verify when a composite lists a remote step then a local one then both are collected",
			discovered: []discoveredAction{
				{key: "org/wrapper@v1", yamls: map[string]string{"": compositeYAMLWithSteps("actions/setup-node@v4", "./scripts/build")}},
				{key: "actions/setup-node@v4"},
			},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("org", "wrapper", "v1", "")}},
			want:      map[string]attribution{"org/wrapper@v1": {}, "actions/setup-node@v4": {parent: "org/wrapper@v1"}},
			wantLocal: []LocalUse{{Raw: "./scripts/build", DeclaredBy: "org/wrapper@v1"}},
		},
		{
			// The local steps are collected on the same walk that attributes Parent.
			name: "verify when a composite lists a local step before a remote one then the remote one keeps its parent",
			discovered: []discoveredAction{
				{key: "org/wrapper@v1", yamls: map[string]string{"": compositeYAMLWithSteps("./scripts/build", "org/child@v1")}},
				{key: "org/child@v1"},
			},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("org", "wrapper", "v1", "")}},
			want:      map[string]attribution{"org/wrapper@v1": {}, "org/child@v1": {parent: "org/wrapper@v1"}},
			wantLocal: []LocalUse{{Raw: "./scripts/build", DeclaredBy: "org/wrapper@v1"}},
		},
		{
			// The root action.yml is not the one invoked, so a local step declared only by the subpath's
			// metadata is exactly the one a root-only read would miss.
			name: "verify when the metadata lives at a subpath then the local step under it is still found",
			discovered: []discoveredAction{
				{key: "github/codeql-action@v3", yamls: map[string]string{
					"":        compositeYAML("org/from-root@v1"),
					"analyze": compositeYAML("./scripts/analyze"),
				}},
				{key: "org/from-root@v1"},
			},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("github", "codeql-action", "v3", "analyze")}},
			want:      map[string]attribution{"github/codeql-action@v3": {subpaths: []string{"analyze"}}, "org/from-root@v1": {}},
			wantLocal: []LocalUse{{Raw: "./scripts/analyze", DeclaredBy: "github/codeql-action@v3"}},
		},
		{
			// What the workflow itself declares is what a reader can act on directly.
			name:       "verify when the job and a composite both declare local steps then the job's comes first",
			discovered: []discoveredAction{{key: "org/wrapper@v1", yamls: map[string]string{"": compositeYAML("./from-composite")}}},
			used: JobUses{
				Remote: []WorkflowUse{remoteUse("org", "wrapper", "v1", "")},
				Local:  []LocalUse{{Raw: "./from-workflow"}},
			},
			want:      map[string]attribution{"org/wrapper@v1": {}},
			wantLocal: []LocalUse{{Raw: "./from-workflow"}, {Raw: "./from-composite", DeclaredBy: "org/wrapper@v1"}},
		},
		{
			// "./x" resolves against the workspace either way, but a reader chasing it needs both places.
			name: "verify when two composites declare the same path then both declarers are reported",
			discovered: []discoveredAction{
				{key: "org/parent-a@v1", yamls: map[string]string{"": compositeYAML("./shared")}},
				{key: "org/parent-b@v1", yamls: map[string]string{"": compositeYAML("./shared")}},
			},
			used:      JobUses{Remote: []WorkflowUse{remoteUse("org", "parent-a", "v1", ""), remoteUse("org", "parent-b", "v1", "")}},
			want:      map[string]attribution{"org/parent-a@v1": {}, "org/parent-b@v1": {}},
			wantLocal: []LocalUse{{Raw: "./shared", DeclaredBy: "org/parent-a@v1"}, {Raw: "./shared", DeclaredBy: "org/parent-b@v1"}},
		},
		{
			name:       "verify when one composite declares the same local step twice then it is reported once",
			discovered: []discoveredAction{{key: "org/wrapper@v1", yamls: map[string]string{"": compositeYAMLWithSteps("./scripts/build", "./scripts/build")}}},
			used:       JobUses{Remote: []WorkflowUse{remoteUse("org", "wrapper", "v1", "")}},
			want:       map[string]attribution{"org/wrapper@v1": {}},
			wantLocal:  []LocalUse{{Raw: "./scripts/build", DeclaredBy: "org/wrapper@v1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotLocal := CrossReference(buildDiscovered(t, tt.discovered), tt.used)

			var gotByKey map[string]attribution
			for _, ref := range got {
				if gotByKey == nil {
					gotByKey = map[string]attribution{}
				}
				gotByKey[ref.Owner+"/"+ref.Repo+"@"+ref.Ref] = attribution{parent: ref.Parent, subpaths: ref.Subpaths}
			}
			assert.Equal(t, tt.want, gotByKey, "CrossReference() attribution")
			assert.Equal(t, tt.wantLocal, gotLocal, "CrossReference() local steps")
		})
	}
}

func TestParseCompositeActionUses(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		why  string
	}{
		{
			name: "verify when the action.yml cannot be parsed then nothing is attributed and the run continues",
			yaml: unreadableYAML,
			why:  "a file this parser cannot read attributes nothing, and is not a failure of the run",
		},
		{
			name: "verify when the action is not composite then nothing is referenced",
			yaml: "runs:\n  using: node20\n",
			why:  "only composite actions declare uses: steps of their own",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "action.yml"), []byte(tt.yaml), 0600))

			got := parseCompositeActionUses(dir, "org/declarer@v1")

			assert.Empty(t, got.Remote, tt.why)
			assert.Empty(t, got.Local, "and no local step either - "+tt.why)
		})
	}
}

func TestCrossReference_SharedChildParentFollowsWorkflowOrder(t *testing.T) {
	discovered := buildDiscovered(t, []discoveredAction{
		{key: "org/parent-a@v1", yamls: map[string]string{"": compositeYAML("org/shared-child@v1")}},
		{key: "org/parent-b@v1", yamls: map[string]string{"": compositeYAML("org/shared-child@v1")}},
		{key: "org/shared-child@v1"},
	})
	used := JobUses{Remote: []WorkflowUse{remoteUse("org", "parent-a", "v1", ""), remoteUse("org", "parent-b", "v1", "")}}

	seen := map[string]bool{}
	for range 200 {
		got, _ := CrossReference(discovered, used)
		for _, ref := range got {
			if ref.Repo == "shared-child" {
				seen[ref.Parent] = true
			}
		}
	}

	assert.Equal(t, map[string]bool{"org/parent-a@v1": true}, seen,
		"when two parents pull in the same child, the first in the file's order must win, every run")
}

// writeWorkflows writes each name->content pair as a file in a fresh temp dir and returns it.
func writeWorkflows(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0600))
	}
	return dir
}

// jobWithUses renders a minimal workflow job declaring one uses: step per ref.
func jobWithUses(jobID string, refs ...string) string {
	job := "  " + jobID + ":\n    steps:\n"
	for _, ref := range refs {
		job += "      - uses: " + ref + "\n"
	}
	return job
}

func TestParseWorkflowUses_SubpathOrderIsStable(t *testing.T) {
	// A monorepo action invoked twice in the same job collapses to one cache entry carrying both
	// subpaths, and that order is rendered into the report's Action cell. Steps are a slice, so
	// the order is the file's - this pins that nothing downstream reintroduces map iteration.
	workflowPath := filepath.Join(writeWorkflows(t, map[string]string{
		"ci.yml": "jobs:\n" + jobWithUses("build",
			"github/codeql-action/init@v3", "github/codeql-action/analyze@v3"),
	}), "ci.yml")

	seen := map[string]bool{}
	for range 200 {
		used, err := ParseWorkflowUses(workflowPath, "build")
		require.NoError(t, err)
		got, _ := CrossReference([]ActionRef{{Owner: "github", Repo: "codeql-action", Ref: "v3", Path: "/nonexistent"}}, used)
		seen[NewActionReportRow(got[0], ActionCurationResult{Status: ActionApproved}).Action] = true
	}
	assert.Equal(t, map[string]bool{"github/codeql-action (init, analyze)": true}, seen,
		"the rendered report cell must follow the file's step order, every run")
}
