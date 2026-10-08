package githubactions

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionsTree lays out an _actions root with one extracted folder and its watermark per "owner/repo/ref"
// and returns the root.
func actionsTree(t *testing.T, refs ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "_actions")
	require.NoError(t, os.MkdirAll(root, 0o755))
	for _, ref := range refs {
		dir := filepath.Join(root, filepath.FromSlash(ref))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "action.yml"), []byte("runs: {}"), 0o644))
		require.NoError(t, os.WriteFile(dir+watermarkSuffix, nil, 0o644))
	}
	return root
}

// folderRef is the ref discovery returns for an extracted folder decided by its content.
func folderRef(root, owner, repo, ref string) ActionRef {
	return ActionRef{Owner: owner, Repo: repo, Ref: ref, Path: filepath.Join(root, owner, repo, filepath.FromSlash(ref))}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestNeutralize(t *testing.T) {
	t.Run("verify when a folder decided by content is rejected then it and its watermark are removed and a sibling is not", func(t *testing.T) {
		root := actionsTree(t, "actions/checkout/v4", "actions/checkout/v3", "actions/setup-go/v5")
		removed, err := Neutralize(root, []ActionRef{folderRef(root, "actions", "checkout", "v4")})
		require.NoError(t, err)
		assert.Equal(t, []string{"actions/checkout@v4"}, removed)
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "v4")))
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "v4.completed")))
		assert.True(t, exists(t, filepath.Join(root, "actions", "checkout", "v3", "action.yml")), "a folder of another ref is decided on its own")
		assert.True(t, exists(t, filepath.Join(root, "actions", "checkout", "v3.completed")))
		assert.True(t, exists(t, filepath.Join(root, "actions", "setup-go", "v5", "action.yml")))
	})
	t.Run("verify when a ref with a slashed branch is rejected then only its folder is removed", func(t *testing.T) {
		root := actionsTree(t, "actions/checkout/copilot/backport-v4", "actions/checkout/v4")
		_, err := Neutralize(root, []ActionRef{folderRef(root, "actions", "checkout", "copilot/backport-v4")})
		require.NoError(t, err)
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "copilot", "backport-v4")))
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "copilot", "backport-v4.completed")))
		assert.True(t, exists(t, filepath.Join(root, "actions", "checkout", "v4")))
	})
	t.Run("verify when a rejected commit is unpaired then every folder of its repository is removed", func(t *testing.T) {
		root := actionsTree(t, "Actions/Checkout/v4", "Actions/Checkout/main", "actions/setup-go/v5")
		removed, err := Neutralize(root, []ActionRef{{Owner: "actions", Repo: "checkout", RunnerSHA: "11d5960a326750d5838078e36cf38b85af677262",
			Verification: VerifyLoggedSHA}})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"Actions/Checkout@v4", "Actions/Checkout@main"}, removed)
		assert.False(t, exists(t, filepath.Join(root, "Actions", "Checkout", "v4")), "the folder spelled in another case holds the same repository")
		assert.False(t, exists(t, filepath.Join(root, "Actions", "Checkout", "main.completed")))
		assert.True(t, exists(t, filepath.Join(root, "Actions", "Checkout")), "the repository folder itself stays")
		assert.True(t, exists(t, filepath.Join(root, "actions", "setup-go", "v5")))
	})
	t.Run("verify when a ref decided by its logged SHA is rejected then every folder of its repository is removed", func(t *testing.T) {
		// The setup line paired the commit with v4; another folder of the repository may hold it all the same.
		root := actionsTree(t, "actions/checkout/v4", "actions/checkout/main")
		ref := folderRef(root, "actions", "checkout", "v4")
		ref.RunnerSHA, ref.Verification = "11d5960a326750d5838078e36cf38b85af677262", VerifyLoggedSHA
		_, err := Neutralize(root, []ActionRef{ref})
		require.NoError(t, err)
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "v4")))
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "main")))
	})
	t.Run("verify when a rejected folder is a symlink into the cache then the link goes and the cache stays", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs a privilege Windows test hosts usually lack")
		}
		root := actionsTree(t)
		cache := filepath.Join(t.TempDir(), "cache", "actions_checkout", "sha")
		require.NoError(t, os.MkdirAll(cache, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(cache, "action.yml"), []byte("runs: {}"), 0o644))
		link := filepath.Join(root, "actions", "checkout", "v4")
		require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
		require.NoError(t, os.Symlink(cache, link))

		_, err := Neutralize(root, []ActionRef{folderRef(root, "actions", "checkout", "v4")})

		require.NoError(t, err)
		assert.False(t, exists(t, link))
		assert.True(t, exists(t, filepath.Join(cache, "action.yml")), "the shared cache must not be emptied through the link")
	})
	t.Run("verify when a whole repository is removed and it holds a symlink then the cache stays", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs a privilege Windows test hosts usually lack")
		}
		root := actionsTree(t, "actions/checkout/main")
		cache := filepath.Join(t.TempDir(), "cache")
		require.NoError(t, os.MkdirAll(cache, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(cache, "action.yml"), []byte("runs: {}"), 0o644))
		require.NoError(t, os.Symlink(cache, filepath.Join(root, "actions", "checkout", "v4")))

		_, err := Neutralize(root, []ActionRef{{Owner: "actions", Repo: "checkout", RunnerSHA: "11d5960a326750d5838078e36cf38b85af677262"}})

		require.NoError(t, err)
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "v4")))
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "main")))
		assert.True(t, exists(t, filepath.Join(cache, "action.yml")))
	})
	t.Run("verify when a path is outside the actions root then it is refused and nothing is removed", func(t *testing.T) {
		root := actionsTree(t, "actions/checkout/v4")
		outside := actionsTree(t, "actions/checkout/v4")
		_, err := Neutralize(root, []ActionRef{folderRef(outside, "actions", "checkout", "v4")})
		require.Error(t, err)
		assert.Contains(t, err.Error(), filepath.Join(outside, "actions", "checkout", "v4"))
		assert.True(t, exists(t, filepath.Join(outside, "actions", "checkout", "v4", "action.yml")))
		assert.True(t, exists(t, filepath.Join(root, "actions", "checkout", "v4", "action.yml")))
	})
	t.Run("verify when a path climbs out of the root through .. then it is refused", func(t *testing.T) {
		root := actionsTree(t, "actions/checkout/v4")
		victim := filepath.Join(filepath.Dir(root), "victim")
		require.NoError(t, os.MkdirAll(victim, 0o755))
		ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4", Path: filepath.Join(root, "actions", "..", "..", "victim")}
		_, err := Neutralize(root, []ActionRef{ref})
		require.Error(t, err)
		assert.True(t, exists(t, victim))
	})
	t.Run("verify when a folder's parent is a symlink out of the root then it is refused", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs a privilege Windows test hosts usually lack")
		}
		root := actionsTree(t)
		elsewhere := actionsTree(t, "actions/checkout/v4")
		require.NoError(t, os.Symlink(filepath.Join(elsewhere, "actions"), filepath.Join(root, "actions")))
		_, err := Neutralize(root, []ActionRef{folderRef(root, "actions", "checkout", "v4")})
		require.Error(t, err)
		assert.True(t, exists(t, filepath.Join(elsewhere, "actions", "checkout", "v4", "action.yml")))
	})
	t.Run("verify when one removal fails then the others still happen and the error names the path", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a directory its owner cannot write, which Windows and root do not honour")
		}
		root := actionsTree(t, "locked/act/v1", "actions/checkout/v4")
		locked := filepath.Join(root, "locked", "act")
		require.NoError(t, os.Chmod(locked, 0o555))
		t.Cleanup(func() { require.NoError(t, os.Chmod(locked, 0o755)) })

		removed, err := Neutralize(root, []ActionRef{folderRef(root, "locked", "act", "v1"), folderRef(root, "actions", "checkout", "v4")})

		require.Error(t, err)
		assert.Contains(t, err.Error(), filepath.Join(locked, "v1"))
		assert.Equal(t, []string{"actions/checkout@v4"}, removed)
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "v4")))
	})
	t.Run("verify when a folder is already gone then it is not an error", func(t *testing.T) {
		root := actionsTree(t)
		removed, err := Neutralize(root, []ActionRef{folderRef(root, "actions", "checkout", "v4")})
		require.NoError(t, err)
		assert.Empty(t, removed)
	})
}

func TestJobFactsFirstStepContinuesOnError(t *testing.T) {
	step := func(name string, coe bool) JobStep {
		return JobStep{Type: "repository", RepositoryType: "GitHub", Name: name, ContinueOnError: coe}
	}
	tests := []struct {
		name          string
		steps         []JobStep
		wantContinues bool
		wantFound     bool
	}{
		{name: "verify when the first step is ours without continue-on-error then the failure stands", steps: []JobStep{step("jfrog/curate", false)}, wantFound: true},
		{name: "verify when the first step is ours with continue-on-error then the failure may be swallowed", steps: []JobStep{step("JFrog/Curate", true)}, wantContinues: true, wantFound: true},
		{name: "verify when another step is first then ours is not found", steps: []JobStep{step("actions/checkout", false), step("jfrog/curate", false)}},
		{name: "verify when the job message has no steps then ours is not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			continues, found := JobFacts{Steps: tt.steps}.FirstStepContinuesOnError("jfrog/curate")
			assert.Equal(t, tt.wantContinues, continues, "FirstStepContinuesOnError() continues")
			assert.Equal(t, tt.wantFound, found, "FirstStepContinuesOnError() found")
		})
	}
}

func TestNeutralizeFallsBackToTheRepository(t *testing.T) {
	// The ref names a folder the runner spelled differently, so its own path removes nothing.
	root := actionsTree(t, "actions/checkout/V4", "actions/setup-go/v5")
	ref := folderRef(root, "actions", "checkout", "v4")
	ref.Path = filepath.Join(root, "actions", "checkout", "v4-gone")
	removed, err := Neutralize(root, []ActionRef{ref})
	require.NoError(t, err)
	assert.Equal(t, []string{"actions/checkout@V4"}, removed)
	assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "V4")))
	assert.True(t, exists(t, filepath.Join(root, "actions", "checkout")))
	assert.True(t, exists(t, filepath.Join(root, "actions", "setup-go", "v5")))
}

func TestNeutralizeQuotesWhatTheActionNamed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a newline in a file name and a directory its owner cannot write")
	}
	root := actionsTree(t, "evil/act/v1")
	dir := filepath.Join(root, "evil", "act", "v1")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "x\n::error::forged"), nil, 0o644))
	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { require.NoError(t, os.Chmod(dir, 0o755)) })

	_, err := Neutralize(root, []ActionRef{folderRef(root, "evil", "act", "v1")})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "\n::error::", "a file name inside the action must not start a line of our output")
	assert.Contains(t, err.Error(), "forged")
}

func TestNeutralizeAll(t *testing.T) {
	t.Run("verify when nothing was decided then every action folder is removed and the owner and repository folders stay", func(t *testing.T) {
		root := actionsTree(t, "actions/checkout/v4", "actions/checkout/copilot/backport", "some/act/v1")
		removed, err := NeutralizeAll(root)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"actions/checkout@v4", "actions/checkout@copilot/backport", "some/act@v1"}, removed)
		for _, gone := range []string{"actions/checkout/v4", "actions/checkout/v4.completed", "actions/checkout/copilot", "some/act/v1"} {
			assert.False(t, exists(t, filepath.Join(root, filepath.FromSlash(gone))), gone)
		}
		assert.True(t, exists(t, filepath.Join(root, "actions", "checkout")))
		assert.True(t, exists(t, root))
	})
	t.Run("verify when an owner folder links out of the root then nothing outside is removed", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("creating a symlink needs a privilege Windows test hosts usually lack")
		}
		root := actionsTree(t, "actions/checkout/v4")
		elsewhere := actionsTree(t, "victim/act/v1")
		require.NoError(t, os.Symlink(filepath.Join(elsewhere, "victim"), filepath.Join(root, "victim")))
		_, err := NeutralizeAll(root)
		require.Error(t, err)
		assert.True(t, exists(t, filepath.Join(elsewhere, "victim", "act", "v1", "action.yml")))
		assert.False(t, exists(t, filepath.Join(root, "actions", "checkout", "v4")))
	})
}

func TestNeutralizeKeepsTheRepositoryFolder(t *testing.T) {
	// A ref folder holding a sub-path action, decided by a commit: the hosted e2e layout.
	root := actionsTree(t, "dattathallam/probe-runner-hook-sha/e2e-v2")
	repoDir := filepath.Join(root, "dattathallam", "probe-runner-hook-sha")
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "e2e-v2", "e2e-victim"), 0o755))
	ref := folderRef(root, "dattathallam", "probe-runner-hook-sha", "e2e-v2")
	ref.RunnerSHA, ref.Verification = "11d5960a326750d5838078e36cf38b85af677262", VerifyLoggedSHA
	unpaired := ActionRef{Owner: "dattathallam", Repo: "probe-runner-hook-sha", RunnerSHA: ref.RunnerSHA, Verification: VerifyLoggedSHA}

	removed, err := Neutralize(root, []ActionRef{ref, unpaired})

	require.NoError(t, err)
	assert.Equal(t, []string{"dattathallam/probe-runner-hook-sha@e2e-v2"}, removed)
	assert.False(t, exists(t, filepath.Join(repoDir, "e2e-v2")))
	assert.False(t, exists(t, filepath.Join(repoDir, "e2e-v2.completed")))
	assert.True(t, exists(t, repoDir))
}
