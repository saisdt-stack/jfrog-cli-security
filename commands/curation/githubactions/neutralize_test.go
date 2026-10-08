package githubactions

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// actionsLayout is a runner's _actions folder and what lies around it, as data. Every path is relative
// to a temp base; the actions root is <base>/_actions, which always exists.
type actionsLayout struct {
	actions  []string          // "owner/repo/ref" folders under _actions, each with an action.yml and its watermark
	dirs     []string          // further directories
	files    map[string]string // further files
	symlinks map[string]string // link -> target
	readOnly []string          // directories their owner may not write, so removing below them fails
}

// build lays the layout out under a fresh temp base. It skips the test where the layout cannot be
// made: a symlink on Windows test hosts, which lack the privilege, and a read-only directory on
// Windows or as root, which do not honour it.
func (l actionsLayout) build(t *testing.T) (base string) {
	t.Helper()
	if len(l.symlinks) > 0 && runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege Windows test hosts usually lack")
	}
	if len(l.readOnly) > 0 && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
		t.Skip("needs a directory its owner cannot write, which Windows and root do not honour")
	}
	base = t.TempDir()
	at := func(rel string) string { return filepath.Join(base, filepath.FromSlash(rel)) }
	require.NoError(t, os.MkdirAll(at("_actions"), 0o755))
	for _, ref := range l.actions {
		dir := at("_actions/" + ref)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "action.yml"), []byte("runs: {}"), 0o644))
		require.NoError(t, os.WriteFile(dir+watermarkSuffix, nil, 0o644))
	}
	for _, dir := range l.dirs {
		require.NoError(t, os.MkdirAll(at(dir), 0o755))
	}
	for name, content := range l.files {
		require.NoError(t, os.MkdirAll(filepath.Dir(at(name)), 0o755))
		require.NoError(t, os.WriteFile(at(name), []byte(content), 0o644))
	}
	for link, target := range l.symlinks {
		require.NoError(t, os.Symlink(at(target), at(link)))
	}
	for _, dir := range l.readOnly {
		require.NoError(t, os.Chmod(at(dir), 0o555))
		t.Cleanup(func() { require.NoError(t, os.Chmod(at(dir), 0o755)) })
	}
	return base
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

// assertRemoval checks what a removal left behind, every path relative to base.
func assertRemoval(t *testing.T, base string, err error, wantErrPath string, gone, kept []string) {
	t.Helper()
	if wantErrPath == "" {
		assert.NoError(t, err)
	} else {
		// Production writes paths with %q (doubling backslashes on Windows), and may report the
		// symlink-resolved base (/private/var on macOS) instead of the one the test holds.
		resolved, evalErr := filepath.EvalSymlinks(base)
		require.NoError(t, evalErr)
		rel := filepath.FromSlash(wantErrPath)
		if assert.Error(t, err) {
			got := err.Error()
			// A path holding ".." is reported as handed in, not cleaned.
			join := filepath.Join
			if strings.Contains(wantErrPath, "..") {
				join = func(elem ...string) string { return elem[0] + string(filepath.Separator) + elem[1] }
			}
			assert.True(t, strings.Contains(got, fmt.Sprintf("%q", join(base, rel))) ||
				strings.Contains(got, fmt.Sprintf("%q", join(resolved, rel))),
				"error %q must name %s", got, wantErrPath)
		}
	}
	for _, rel := range gone {
		assert.False(t, exists(t, filepath.Join(base, filepath.FromSlash(rel))), "%s must be removed", rel)
	}
	for _, rel := range kept {
		assert.True(t, exists(t, filepath.Join(base, filepath.FromSlash(rel))), "%s must stay", rel)
	}
}

func TestNeutralize(t *testing.T) {
	tests := []struct {
		name   string
		layout actionsLayout
		// refs are the rejected actions; a Path is relative to the layout's base.
		refs        []ActionRef
		wantRemoved []string // "owner/repo@ref", in any order
		wantGone    []string
		wantKept    []string
		// wantErrPath is the path the error must name, relative to the base; "" means no error.
		wantErrPath string
	}{
		{
			name:        "verify when a folder decided by content is rejected then it and its watermark are removed and a sibling is not",
			layout:      actionsLayout{actions: []string{"actions/checkout/v4", "actions/checkout/v3", "actions/setup-go/v5"}},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4"}},
			wantRemoved: []string{"actions/checkout@v4"},
			wantGone:    []string{"_actions/actions/checkout/v4", "_actions/actions/checkout/v4.completed"},
			// A folder of another ref is decided on its own.
			wantKept: []string{"_actions/actions/checkout/v3/action.yml", "_actions/actions/checkout/v3.completed", "_actions/actions/setup-go/v5/action.yml"},
		},
		{
			name:        "verify when a ref with a slashed branch is rejected then only its folder is removed",
			layout:      actionsLayout{actions: []string{"actions/checkout/copilot/backport-v4", "actions/checkout/v4"}},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "copilot/backport-v4", Path: "_actions/actions/checkout/copilot/backport-v4"}},
			wantRemoved: []string{"actions/checkout@copilot/backport-v4"},
			wantGone:    []string{"_actions/actions/checkout/copilot/backport-v4", "_actions/actions/checkout/copilot/backport-v4.completed"},
			wantKept:    []string{"_actions/actions/checkout/v4"},
		},
		{
			// The folder spelled in another case holds the same repository; the repository folder itself stays.
			name:        "verify when a rejected commit is unpaired then every folder of its repository is removed",
			layout:      actionsLayout{actions: []string{"Actions/Checkout/v4", "Actions/Checkout/main", "actions/setup-go/v5"}},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", RunnerSHA: shaV4, Verification: VerifyLoggedSHA}},
			wantRemoved: []string{"Actions/Checkout@v4", "Actions/Checkout@main"},
			wantGone:    []string{"_actions/Actions/Checkout/v4", "_actions/Actions/Checkout/main.completed"},
			wantKept:    []string{"_actions/Actions/Checkout", "_actions/actions/setup-go/v5"},
		},
		{
			// The setup line paired the commit with v4; another folder of the repository may hold it all the same.
			name:   "verify when a ref decided by its logged SHA is rejected then every folder of its repository is removed",
			layout: actionsLayout{actions: []string{"actions/checkout/v4", "actions/checkout/main"}},
			refs: []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4",
				RunnerSHA: shaV4, Verification: VerifyLoggedSHA}},
			wantRemoved: []string{"actions/checkout@v4", "actions/checkout@main"},
			wantGone:    []string{"_actions/actions/checkout/v4", "_actions/actions/checkout/main"},
		},
		{
			// A hosted e2e layout: a ref folder holding a sub-path action, rejected both paired and unpaired.
			name: "verify when a paired and an unpaired ref of one repository are rejected then the folder is removed once and the repository folder stays",
			layout: actionsLayout{
				actions: []string{"dattathallam/probe-runner-hook-sha/e2e-v2"},
				dirs:    []string{"_actions/dattathallam/probe-runner-hook-sha/e2e-v2/e2e-victim"},
			},
			refs: []ActionRef{
				{Owner: "dattathallam", Repo: "probe-runner-hook-sha", Ref: "e2e-v2", Path: "_actions/dattathallam/probe-runner-hook-sha/e2e-v2",
					RunnerSHA: shaV4, Verification: VerifyLoggedSHA},
				{Owner: "dattathallam", Repo: "probe-runner-hook-sha", RunnerSHA: shaV4, Verification: VerifyLoggedSHA},
			},
			wantRemoved: []string{"dattathallam/probe-runner-hook-sha@e2e-v2"},
			wantGone:    []string{"_actions/dattathallam/probe-runner-hook-sha/e2e-v2", "_actions/dattathallam/probe-runner-hook-sha/e2e-v2.completed"},
			wantKept:    []string{"_actions/dattathallam/probe-runner-hook-sha"},
		},
		{
			// The ref names a folder the runner spelled differently, so its own path removes nothing.
			name:        "verify when a rejected folder is not where the ref says then the repository's folders are removed instead",
			layout:      actionsLayout{actions: []string{"actions/checkout/V4", "actions/setup-go/v5"}},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4-gone"}},
			wantRemoved: []string{"actions/checkout@V4"},
			wantGone:    []string{"_actions/actions/checkout/V4"},
			wantKept:    []string{"_actions/actions/checkout", "_actions/actions/setup-go/v5"},
		},
		{
			name: "verify when a folder is already gone then it is not an error",
			refs: []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4"}},
		},
		{
			name: "verify when a rejected folder is a symlink into the cache then the link goes and the cache stays",
			layout: actionsLayout{
				dirs:     []string{"_actions/actions/checkout"},
				files:    map[string]string{"cache/actions_checkout/sha/action.yml": "runs: {}"},
				symlinks: map[string]string{"_actions/actions/checkout/v4": "cache/actions_checkout/sha"},
			},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4"}},
			wantRemoved: []string{"actions/checkout@v4"},
			wantGone:    []string{"_actions/actions/checkout/v4"},
			// The shared cache must not be emptied through the link.
			wantKept: []string{"cache/actions_checkout/sha/action.yml"},
		},
		{
			name: "verify when a whole repository is removed and it holds a symlink then the cache stays",
			layout: actionsLayout{
				actions:  []string{"actions/checkout/main"},
				files:    map[string]string{"cache/action.yml": "runs: {}"},
				symlinks: map[string]string{"_actions/actions/checkout/v4": "cache"},
			},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", RunnerSHA: shaV4}},
			wantRemoved: []string{"actions/checkout@main", "actions/checkout@v4"},
			wantGone:    []string{"_actions/actions/checkout/v4", "_actions/actions/checkout/main"},
			wantKept:    []string{"cache/action.yml"},
		},
		{
			name: "verify when a path is outside the actions root then it is refused and nothing is removed",
			layout: actionsLayout{
				actions: []string{"actions/checkout/v4"},
				files:   map[string]string{"outside/actions/checkout/v4/action.yml": "runs: {}"},
			},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "outside/actions/checkout/v4"}},
			wantErrPath: "outside/actions/checkout/v4",
			wantKept:    []string{"outside/actions/checkout/v4/action.yml", "_actions/actions/checkout/v4/action.yml"},
		},
		{
			name: "verify when a path climbs out of the root through .. then it is refused",
			layout: actionsLayout{
				actions: []string{"actions/checkout/v4"},
				dirs:    []string{"victim"},
			},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/../../victim"}},
			wantErrPath: "_actions/actions/../../victim",
			wantKept:    []string{"victim"},
		},
		{
			name: "verify when a folder's parent is a symlink out of the root then it is refused",
			layout: actionsLayout{
				files:    map[string]string{"elsewhere/actions/checkout/v4/action.yml": "runs: {}"},
				symlinks: map[string]string{"_actions/actions": "elsewhere/actions"},
			},
			refs:        []ActionRef{{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4"}},
			wantErrPath: "_actions/actions/checkout/v4",
			wantKept:    []string{"elsewhere/actions/checkout/v4/action.yml"},
		},
		{
			name: "verify when one removal fails then the others still happen and the error names the path",
			layout: actionsLayout{
				actions:  []string{"locked/act/v1", "actions/checkout/v4"},
				readOnly: []string{"_actions/locked/act"},
			},
			refs: []ActionRef{
				{Owner: "locked", Repo: "act", Ref: "v1", Path: "_actions/locked/act/v1"},
				{Owner: "actions", Repo: "checkout", Ref: "v4", Path: "_actions/actions/checkout/v4"},
			},
			wantRemoved: []string{"actions/checkout@v4"},
			wantGone:    []string{"_actions/actions/checkout/v4"},
			wantErrPath: "_actions/locked/act/v1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.layout.build(t)
			refs := make([]ActionRef, len(tt.refs))
			for i, ref := range tt.refs {
				switch {
				case strings.Contains(ref.Path, ".."):
					// filepath.Join would clean the climb away before Neutralize saw it.
					ref.Path = base + string(filepath.Separator) + filepath.FromSlash(ref.Path)
				case ref.Path != "":
					ref.Path = filepath.Join(base, filepath.FromSlash(ref.Path))
				}
				refs[i] = ref
			}

			removed, err := Neutralize(filepath.Join(base, "_actions"), refs)

			assert.ElementsMatch(t, tt.wantRemoved, removed, "Neutralize() removed")
			assertRemoval(t, base, err, tt.wantErrPath, tt.wantGone, tt.wantKept)
		})
	}
}

func TestNeutralizeAll(t *testing.T) {
	tests := []struct {
		name        string
		layout      actionsLayout
		wantRemoved []string
		wantGone    []string
		wantKept    []string
		wantErrPath string
	}{
		{
			name:        "verify when nothing was decided then every action folder is removed and the owner and repository folders stay",
			layout:      actionsLayout{actions: []string{"actions/checkout/v4", "actions/checkout/copilot/backport", "some/act/v1"}},
			wantRemoved: []string{"actions/checkout@v4", "actions/checkout@copilot/backport", "some/act@v1"},
			wantGone:    []string{"_actions/actions/checkout/v4", "_actions/actions/checkout/v4.completed", "_actions/actions/checkout/copilot", "_actions/some/act/v1"},
			wantKept:    []string{"_actions/actions/checkout", "_actions"},
		},
		{
			name: "verify when an owner folder links out of the root then nothing outside is removed",
			layout: actionsLayout{
				actions:  []string{"actions/checkout/v4"},
				files:    map[string]string{"elsewhere/victim/act/v1/action.yml": "runs: {}", "elsewhere/victim/act/v1.completed": ""},
				symlinks: map[string]string{"_actions/victim": "elsewhere/victim"},
			},
			wantRemoved: []string{"actions/checkout@v4"},
			wantGone:    []string{"_actions/actions/checkout/v4"},
			wantKept:    []string{"elsewhere/victim/act/v1/action.yml"},
			wantErrPath: "_actions/victim/act/v1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.layout.build(t)

			removed, err := NeutralizeAll(filepath.Join(base, "_actions"))

			assert.ElementsMatch(t, tt.wantRemoved, removed, "NeutralizeAll() removed")
			assertRemoval(t, base, err, tt.wantErrPath, tt.wantGone, tt.wantKept)
		})
	}
}

func TestNeutralizeQuotesWhatTheActionNamed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a newline in a file name and a directory its owner cannot write")
	}
	base := actionsLayout{
		actions:  []string{"evil/act/v1"},
		files:    map[string]string{"_actions/evil/act/v1/x\n::error::forged": ""},
		readOnly: []string{"_actions/evil/act/v1"},
	}.build(t)
	root := filepath.Join(base, "_actions")

	_, err := Neutralize(root, []ActionRef{{Owner: "evil", Repo: "act", Ref: "v1", Path: filepath.Join(root, "evil", "act", "v1")}})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "\n::error::", "a file name inside the action must not start a line of our output")
	assert.Contains(t, err.Error(), "forged")
}
