package githubactions

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cacheNode is one path of an archive cache laid out by archiveCache, relative to its root.
type cacheNode struct {
	path    string
	mode    fs.FileMode // 0 keeps the default 0755 or 0644
	dir     bool
	symlink string // when set, the node is a symlink to this target and mode is ignored
}

// archiveCache lays out nodes under a fresh root, then applies each non-zero mode, deepest first, and
// a non-zero rootMode to the root. Write permission is restored on cleanup so the temp dir can be
// removed.
func archiveCache(t *testing.T, rootMode fs.FileMode, nodes ...cacheNode) string {
	t.Helper()
	root := t.TempDir()
	t.Cleanup(func() {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.Type()&fs.ModeSymlink != 0 {
				return err
			}
			return os.Chmod(path, 0o700)
		}); err != nil {
			t.Logf("restoring write permission under %s: %v", root, err)
		}
	})
	for _, n := range nodes {
		path := filepath.Join(root, filepath.FromSlash(n.path))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		switch {
		case n.symlink != "":
			require.NoError(t, os.Symlink(n.symlink, path))
		case n.dir:
			require.NoError(t, os.MkdirAll(path, 0o755))
		default:
			require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
		}
	}
	deepestFirst := slices.Clone(nodes)
	slices.SortFunc(deepestFirst, func(a, b cacheNode) int { return strings.Count(b.path, "/") - strings.Count(a.path, "/") })
	for _, n := range deepestFirst {
		if n.symlink == "" && n.mode != 0 {
			require.NoError(t, os.Chmod(filepath.Join(root, filepath.FromSlash(n.path)), n.mode))
		}
	}
	if rootMode != 0 {
		require.NoError(t, os.Chmod(root, rootMode))
	}
	return root
}

func skipOnWindowsOrAsRoot(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("CacheEntryReadOnly does not check Windows ACLs and always returns false there")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can write any path, so nothing is locked to it")
	}
}

// lockAllBut makes pathLocked report every path under root locked except those in unlocked, given
// relative to root ("." is root itself), for the rest of the test.
func lockAllBut(t *testing.T, root string, unlocked ...string) {
	t.Helper()
	previous := pathLocked
	t.Cleanup(func() { pathLocked = previous })
	pathLocked = func(path string) bool {
		rel, err := filepath.Rel(root, path)
		return err == nil && !strings.HasPrefix(rel, "..") && !slices.Contains(unlocked, filepath.ToSlash(rel))
	}
}

var cachedCheckout = WorkerAction{Owner: "actions", Repo: "checkout", SHA: shaV4, Source: SourceCache}

// TestCacheEntryReadOnlyOnDisk runs the real per-path check. Every path a test can create is owned by
// the user running it, who can always chmod it back, so on disk only "not trusted" can be shown: even a
// cache the job's user made read-only is not trusted.
func TestCacheEntryReadOnlyOnDisk(t *testing.T) {
	skipOnWindowsOrAsRoot(t)
	root := archiveCache(t, 0o555,
		cacheNode{path: "actions_checkout", dir: true, mode: 0o555},
		cacheNode{path: "actions_checkout/" + shaV4 + ".tar.gz", mode: 0o444})

	assert.False(t, CacheEntryReadOnly(root, cachedCheckout), "CacheEntryReadOnly(%q, %v) of a cache the job's user owns", root, cachedCheckout)
}

// TestCacheEntryReadOnlyChecksEveryPath stands in for the per-path check, so that the paths it is
// asked about - the located entries, their trees and their parents up to the cache root - can be
// tested without a cache another user owns, and as any user on any OS.
func TestCacheEntryReadOnlyChecksEveryPath(t *testing.T) {
	archive := "actions_checkout/" + shaV4 + ".tar.gz"
	unpacked := "actions_checkout/" + shaV4
	unpackedTree := []cacheNode{
		{path: unpacked + "/checkout-" + shaV4 + "/dist", dir: true},
		{path: unpacked + "/checkout-" + shaV4 + "/dist/index.js"},
	}
	tests := []struct {
		name     string
		nodes    []cacheNode
		unlocked []string
		want     bool
	}{
		{
			name:  "verify when the archive, its folder and the cache root are locked then the entry is trusted",
			nodes: []cacheNode{{path: archive}},
			want:  true,
		},
		{
			name:     "verify when the cache root is not locked then the entry folder can be swapped and is not trusted",
			nodes:    []cacheNode{{path: archive}},
			unlocked: []string{"."},
		},
		{
			name:     "verify when the action's folder is not locked then the archive can be replaced and is not trusted",
			nodes:    []cacheNode{{path: archive}},
			unlocked: []string{"actions_checkout"},
		},
		{
			name:     "verify when the archive itself is not locked then it is not trusted",
			nodes:    []cacheNode{{path: archive}},
			unlocked: []string{archive},
		},
		{
			name:  "verify when the folder is named in another case then the entry is still found",
			nodes: []cacheNode{{path: "Actions_Checkout/" + shaV4 + ".tar.gz"}},
			want:  true,
		},
		{
			name:  "verify when the cache holds no entry for the action then it is not trusted",
			nodes: []cacheNode{{path: "actions_checkout/" + shaV3 + ".tar.gz"}},
		},
		{
			name:  "verify when an unpacked symlink-cache directory is locked throughout then it is trusted",
			nodes: unpackedTree,
			want:  true,
		},
		{
			name:     "verify when one nested file of an unpacked directory is not locked then the job can edit it through the symlink",
			nodes:    unpackedTree,
			unlocked: []string{unpacked + "/checkout-" + shaV4 + "/dist/index.js"},
		},
		{
			name:  "verify when an unpacked directory holds a symlink then its target, outside the check, makes it untrusted",
			nodes: []cacheNode{{path: unpacked + "/checkout-" + shaV4 + "/dist", symlink: os.TempDir()}},
		},
		{
			name:  "verify when the action's folder is a symlink then it is not trusted",
			nodes: []cacheNode{{path: "real/" + shaV4 + ".tar.gz"}, {path: "actions_checkout", symlink: "real"}},
		},
		{
			name:     "verify when both an archive and an unpacked directory exist then the one not locked makes it untrusted",
			nodes:    append([]cacheNode{{path: archive}}, unpackedTree...),
			unlocked: []string{unpacked + "/checkout-" + shaV4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && slices.ContainsFunc(tt.nodes, func(n cacheNode) bool { return n.symlink != "" }) {
				t.Skip("creating a symlink on Windows needs a privilege tests do not have")
			}
			root := archiveCache(t, 0, tt.nodes...)
			lockAllBut(t, root, tt.unlocked...)
			assert.Equal(t, tt.want, CacheEntryReadOnly(root, cachedCheckout), "CacheEntryReadOnly(%q, %v)", root, cachedCheckout)
		})
	}
}

func TestCacheEntryReadOnlyNeedsAnAbsoluteCacheDir(t *testing.T) {
	root := archiveCache(t, 0, cacheNode{path: "actions_checkout/" + shaV4 + ".tar.gz"})
	lockAllBut(t, root)
	require.True(t, CacheEntryReadOnly(root, cachedCheckout), "the fixture itself must be trusted")
	t.Chdir(filepath.Dir(root))
	assert.False(t, CacheEntryReadOnly(filepath.Base(root), cachedCheckout), "a relative directory depends on where the check runs")
	assert.False(t, CacheEntryReadOnly("", cachedCheckout), "the runner named no cache directory")
}
