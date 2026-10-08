package githubactions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeGitFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
}

func TestWorkspaceCommit(t *testing.T) {
	const sha = "ac28ebad35b91c09ec81aab0ceeb2517d0c96145"
	tests := []struct {
		name  string
		files map[string]string // checkout-relative name -> content
		// gitdir, when set, makes .git a file naming this checkout-relative directory by its absolute path.
		gitdir string
		want   string // "" means there is no commit to read and the call fails
	}{
		{
			name:  "verify when HEAD names a loose branch ref then its commit is read",
			files: map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": sha + "\n"},
			want:  sha,
		},
		{
			name:  "verify when HEAD is detached then it is the commit",
			files: map[string]string{".git/HEAD": sha + "\n"},
			want:  sha,
		},
		{
			name:  "verify when the branch ref is packed then packed-refs is read",
			files: map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/packed-refs": "# pack-refs with: peeled fully-peeled sorted\n" + sha + " refs/heads/main\n"},
			want:  sha,
		},
		{
			name:   "verify when .git is a file pointing elsewhere then that directory is read",
			files:  map[string]string{"real-git/HEAD": sha + "\n"},
			gitdir: "real-git",
			want:   sha,
		},
		{
			name: "verify when there is no checkout then it fails",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeGitFiles(t, dir, tt.files)
			if tt.gitdir != "" {
				writeGitFiles(t, dir, map[string]string{".git": "gitdir: " + filepath.Join(dir, tt.gitdir) + "\n"})
			}

			got, err := WorkspaceCommit(dir)

			if tt.want == "" {
				assert.Error(t, err, "WorkspaceCommit() = %q", got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
