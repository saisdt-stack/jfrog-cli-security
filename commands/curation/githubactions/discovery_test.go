package githubactions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixturesRoot = "../../../tests/testdata/projects/githubactions"

// symlinkOrSkip links newname -> oldname, skipping the test where the OS won't allow it
// (Windows needs privileges for symlink creation).
func symlinkOrSkip(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		t.Skipf("cannot create symlinks on this platform: %v", err)
	}
}

// actionCache is the cache a discovery case starts from, expressed as data. Either a checked-in
// fixture, or a tree built under a temp base - where the cache root is <base>/_actions and every
// other path is relative to <base>, so a symlink target can sit outside the cache the way the
// runner's archive cache does.
type actionCache struct {
	fixture  string            // a project under fixturesRoot; its _work/_actions is the cache
	dirs     []string          // directories to create, relative to <base>
	files    map[string]string // files to write, relative to <base>
	symlinks map[string]string // link path -> target path, both relative to <base>
}

func (c actionCache) build(t *testing.T) (cacheRoot string) {
	t.Helper()
	if c.fixture != "" {
		return filepath.Join(fixturesRoot, c.fixture, "_work", "_actions")
	}
	base := t.TempDir()
	for _, dir := range c.dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(base, filepath.FromSlash(dir)), 0755))
	}
	for name, content := range c.files {
		require.NoError(t, os.WriteFile(filepath.Join(base, filepath.FromSlash(name)), []byte(content), 0600))
	}
	for link, target := range c.symlinks {
		symlinkOrSkip(t, filepath.Join(base, filepath.FromSlash(target)), filepath.Join(base, filepath.FromSlash(link)))
	}
	return filepath.Join(base, "_actions")
}

func TestDiscoverActionCache(t *testing.T) {
	tests := []struct {
		name        string
		cache       actionCache
		wantEntries []string
	}{
		{
			name:        "verify when the cache is well-formed then one entry per owner repo ref is returned",
			cache:       actionCache{fixture: "curation-project"},
			wantEntries: []string{"actions/checkout@v4", "github/codeql-action@v3", "some-org/transitive-action@v1"},
		},
		{
			name:  "verify when the cache directory does not exist then the result is empty and no error",
			cache: actionCache{fixture: "does-not-exist"},
		},
		{
			// stray-file.txt (owner level), actions/stray-file-at-repo-level.txt (repo level) and
			// onlyowner/ (an owner dir with no repo subdirectories).
			name:        "verify when an entry is not a well-formed triple then it is skipped without error",
			cache:       actionCache{fixture: "malformed-project"},
			wantEntries: []string{"actions/checkout@v4"},
		},
		{
			// What ACTIONS_RUNNER_SYMLINK_CACHED_ACTIONS produces, linked here at each of the
			// three levels the walk descends.
			name: "verify when entries are symlinked into the archive cache then they are followed",
			cache: actionCache{
				dirs: []string{
					"_actions/actions/checkout/v4", // a cache miss: extracted, so a real directory
					"_actions/actions/setup-node",
					"archive-cache/setup-node-v4",
					"archive-cache/cache-repo/v3",
					"archive-cache/github-owner/codeql-action/v3",
				},
				// The watermark the runner drops beside an extracted entry is still not an action.
				files: map[string]string{"_actions/actions/checkout/v4.completed": "ts"},
				symlinks: map[string]string{
					"_actions/actions/setup-node/v4": "archive-cache/setup-node-v4", // ref level
					"_actions/actions/cache":         "archive-cache/cache-repo",    // repo level
					"_actions/github":                "archive-cache/github-owner",  // owner level
				},
			},
			wantEntries: []string{"actions/checkout@v4", "actions/setup-node@v4", "actions/cache@v3", "github/codeql-action@v3"},
		},
		{
			name: "verify when a symlink resolves to nothing then it is skipped and the walk continues",
			cache: actionCache{
				dirs:     []string{"_actions/actions/checkout"},
				symlinks: map[string]string{"_actions/actions/checkout/v4": "nowhere"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheRoot := tt.cache.build(t)

			refs, err := DiscoverActionCache(cacheRoot)

			assert.NoError(t, err)
			assert.NotNil(t, refs, "an empty result must still be a usable slice")
			found := make([]string, len(refs))
			for i, ref := range refs {
				found[i] = ref.Owner + "/" + ref.Repo + "@" + ref.Ref
			}
			assert.ElementsMatch(t, tt.wantEntries, found)
			for _, ref := range refs {
				assert.Equal(t, filepath.Join(cacheRoot, ref.Owner, ref.Repo, ref.Ref), ref.Path)
				assert.Empty(t, ref.Subpaths, "DiscoverActionCache must not set Subpaths - that's CrossReference's job")
				assert.Empty(t, ref.Parent, "DiscoverActionCache must not set Parent - that's CrossReference's job")
			}
		})
	}
}

func TestDefaultActionsCacheDir(t *testing.T) {
	tests := []struct {
		name            string
		runnerWorkspace string
		want            string
		wantErr         bool
	}{
		{
			name:            "verify when RUNNER_WORKSPACE is set then the cache path is its sibling",
			runnerWorkspace: "/home/runner/work/my-repo",
			want:            filepath.Clean("/home/runner/work/_actions"),
		},
		{
			name:            "verify when RUNNER_WORKSPACE is unset then an error is returned rather than a guess",
			runnerWorkspace: "",
			wantErr:         true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(RunnerWorkspaceEnvVar, tt.runnerWorkspace)

			dir, err := DefaultActionsCacheDir()

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, dir)
		})
	}
}

func TestDefaultWorkflowFile(t *testing.T) {
	tests := []struct {
		name        string
		workflowRef string
		want        string
	}{
		{"verify when the ref is standard then the repo-relative path is returned", "octocat/hello-world/.github/workflows/ci.yml@refs/heads/main", ".github/workflows/ci.yml"},
		{"verify when the ref contains slashes then the path is still returned", "octocat/hello-world/.github/workflows/ci.yml@refs/heads/my/feature", ".github/workflows/ci.yml"},
		{"verify when the ref is a tag then the path is still returned", "octocat/hello-world/.github/workflows/release.yaml@refs/tags/v1.2.3", ".github/workflows/release.yaml"},
		{"verify when the variable is unset then the path is empty", "", ""},
		{"verify when the value carries no path then the result is empty", "octocat/hello-world@refs/heads/main", ""},
		{"verify when the value has no ref suffix then the path is still returned", "octocat/hello-world/.github/workflows/ci.yml", ".github/workflows/ci.yml"},
		{"verify when the value is malformed then the result is empty rather than an error", "nonsense", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(WorkflowRefEnvVar, tt.workflowRef)
			assert.Equal(t, tt.want, DefaultWorkflowFile())
		})
	}
}

func TestDefaultJobID(t *testing.T) {
	t.Setenv(JobIDEnvVar, "build")
	assert.Equal(t, "build", DefaultJobID())

	t.Setenv(JobIDEnvVar, "")
	assert.Empty(t, DefaultJobID())
}

func TestExcludeDeliveryAction(t *testing.T) {
	tests := []struct {
		name      string
		refs      []ActionRef
		wantRepos []string
	}{
		{
			name: "verify when the delivery action is present at any ref then it is dropped",
			refs: []ActionRef{
				{Owner: "jfrog", Repo: "setup-jfrog-cli", Ref: "v4"},
				{Owner: "jfrog", Repo: "setup-jfrog-cli", Ref: "9a4c2881"},
				{Owner: "actions", Repo: "checkout", Ref: "v4"},
			},
			wantRepos: []string{"checkout"},
		},
		{
			name: "verify when another jfrog action is present then it is kept",
			refs: []ActionRef{
				{Owner: "jfrog", Repo: "frogbot", Ref: "v2"},
				{Owner: "jfrog", Repo: "setup-jfrog-cli", Ref: "v4"},
			},
			wantRepos: []string{"frogbot"},
		},
		{
			name: "verify when another owner ships a same-named action then it is kept",
			refs: []ActionRef{
				{Owner: "not-jfrog", Repo: "setup-jfrog-cli", Ref: "v4"},
			},
			wantRepos: []string{"setup-jfrog-cli"},
		},
		{
			name:      "verify when only the delivery action is present then nothing is left to curate",
			refs:      []ActionRef{{Owner: "jfrog", Repo: "setup-jfrog-cli", Ref: "v4"}},
			wantRepos: nil,
		},
		{
			name:      "verify when there are no refs then the result stays empty",
			refs:      nil,
			wantRepos: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kept := ExcludeDeliveryAction(tt.refs)
			repos := make([]string, len(kept))
			for i, ref := range kept {
				repos[i] = ref.Repo
			}
			if tt.wantRepos == nil {
				assert.Empty(t, repos)
				return
			}
			assert.Equal(t, tt.wantRepos, repos)
		})
	}
}

func TestExcludeDeliveryAction_PreservesTransitiveAttribution(t *testing.T) {
	// Excluding the delivery action must not orphan anything it pulled in: attribution runs
	// before this filter, so a child keeps its Parent even though that parent is not reported.
	kept := ExcludeDeliveryAction([]ActionRef{
		{Owner: "jfrog", Repo: "setup-jfrog-cli", Ref: "v4"},
		{Owner: "some-org", Repo: "pulled-in-by-delivery", Ref: "v1", Parent: "jfrog/setup-jfrog-cli@v4"},
	})

	if assert.Len(t, kept, 1) {
		assert.Equal(t, "pulled-in-by-delivery", kept[0].Repo)
		assert.Equal(t, "jfrog/setup-jfrog-cli@v4", kept[0].Parent, "attribution must survive the exclusion")
	}
}
