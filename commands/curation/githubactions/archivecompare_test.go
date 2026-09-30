package githubactions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tarEntry is one entry of a test archive: a file with content, a directory, a symlink, or - when
// pax is set - a pax global header carrying those records, as GitHub writes first.
type tarEntry struct {
	name     string
	content  string
	dir      bool
	linkname string
	pax      map[string]string
}

func buildTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.content)), Typeflag: tar.TypeReg}
		switch {
		case e.dir:
			hdr = &tar.Header{Name: e.name, Mode: 0o755, Typeflag: tar.TypeDir}
		case e.linkname != "":
			hdr = &tar.Header{Name: e.name, Linkname: e.linkname, Mode: 0o777, Typeflag: tar.TypeSymlink}
		case e.pax != nil:
			hdr = &tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: e.pax, Format: tar.FormatPAX}
		}
		require.NoError(t, tw.WriteHeader(hdr))
		if hdr.Typeflag == tar.TypeReg {
			_, err := tw.Write([]byte(e.content))
			require.NoError(t, err)
		}
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// actionArchive is a well-formed archive shaped like Artifactory's: one top-level directory.
func actionArchive(t *testing.T) []byte {
	t.Helper()
	return buildTarGz(t,
		tarEntry{name: "checkout-" + branchTip + "/", dir: true},
		tarEntry{name: "checkout-" + branchTip + "/action.yml", content: "name: curated\n"},
		tarEntry{name: "checkout-" + branchTip + "/dist/", dir: true},
		tarEntry{name: "checkout-" + branchTip + "/dist/index.js", content: "curated();\n"},
	)
}

// runnerCache lays out _actions/actions/checkout/<ref> holding exactly actionArchive's content plus
// a file a pre step wrote there, with the runner's "<last ref segment>.completed" marker beside it,
// and returns the ref directory.
func runnerCache(t *testing.T, ref string) string {
	t.Helper()
	actionDir := filepath.Join(t.TempDir(), "_actions", "actions", "checkout", filepath.FromSlash(ref))
	require.NoError(t, os.MkdirAll(filepath.Join(actionDir, "dist"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(actionDir, "action.yml"), []byte("name: curated\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(actionDir, "dist", "index.js"), []byte("curated();\n"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(actionDir, ".pre-ran"), nil, 0o644))
	require.NoError(t, os.WriteFile(actionDir+watermarkSuffix, nil, 0o644))
	return actionDir
}

// snapshotDir records every entry below dir - a file's content and executable bit, a symlink's
// target - so a test can prove dir was left exactly as it was. The runner's .completed marker beside
// dir is included.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			snapshot[rel] = "link:" + target
			return err
		case info.IsDir():
			snapshot[rel] = "dir"
		default:
			content, err := os.ReadFile(path)
			snapshot[rel] = fmt.Sprintf("file exec=%t:%s", info.Mode()&0o111 != 0, content)
			return err
		}
		return nil
	}))
	_, err := os.Stat(dir + watermarkSuffix)
	snapshot["<completed marker>"] = fmt.Sprintf("present=%t", err == nil)
	return snapshot
}

// compareTop is the single top-level directory GitHub wraps an archive's content in.
const compareTop = "checkout-" + branchTip + "/"

// runnerLayout is an action directory as the runner left it, described as data.
type runnerLayout struct {
	files map[string]string
	links map[string]string
	dirs  []string
}

func (l runnerLayout) build(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range l.dirs {
		require.NoError(t, os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o755))
	}
	for name, content := range l.files {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	for name, target := range l.links {
		path := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.Symlink(target, path))
	}
	return root
}

// writeTestArchive writes content to a file and returns its path.
func writeTestArchive(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "action.tar.gz")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func TestCompareArchive(t *testing.T) {
	topDir := tarEntry{name: compareTop, dir: true}
	actionYML := tarEntry{name: compareTop + "action.yml", content: "name: curated\n"}
	distDir := tarEntry{name: compareTop + "dist/", dir: true}
	indexJS := tarEntry{name: compareTop + "dist/index.js", content: "curated();\n"}
	matching := map[string]string{"action.yml": "name: curated\n", "dist/index.js": "curated();\n"}
	fileLink := tarEntry{name: compareTop + "link.yml", linkname: "action.yml"}
	// dist sorts before lib, so the link arrives before the directory it points at.
	dirLink := tarEntry{name: compareTop + "dist", linkname: "lib"}
	libDir := tarEntry{name: compareTop + "lib/", dir: true}
	libJS := tarEntry{name: compareTop + "lib/index.js", content: "lib();\n"}

	tests := []struct {
		name          string
		archive       []tarEntry
		rawArchive    []byte
		runner        runnerLayout
		rootIsSymlink bool
		// linksAsText runs the row as on a runner that unzips a symlink into a file holding the link
		// path - Windows.
		linksAsText bool
		want        ArchiveComparison
		wantErr     bool
	}{
		{
			name:    "verify when the runner holds the archive's files then they are identical",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: matching},
			want:    ArchiveComparison{Identical: true},
		},
		{
			// A pre step or the action itself writes into its own directory (F4).
			name:    "verify when the runner holds an extra file then they are still identical",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n", "dist/index.js": "curated();\n", ".pre-ran": ""}},
			want:    ArchiveComparison{Identical: true},
		},
		{
			name:    "verify when a file is missing on the runner then it is reported missing",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n"}},
			want:    ArchiveComparison{FirstDifference: "dist/index.js", Missing: true},
		},
		{
			name:    "verify when a file has the same size and other bytes then it differs",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: CURATED\n", "dist/index.js": "curated();\n"}},
			want:    ArchiveComparison{FirstDifference: "action.yml"},
		},
		{
			name:    "verify when a file has another size then it differs",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: moved\n", "dist/index.js": "curated();\n"}},
			want:    ArchiveComparison{FirstDifference: "action.yml"},
		},
		{
			name:    "verify when the runner has a directory where the archive has a file then it differs",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n"}, dirs: []string{"dist/index.js"}},
			want:    ArchiveComparison{FirstDifference: "dist/index.js"},
		},
		{
			name:    "verify when the parent of a file is a file on the runner then it is reported missing",
			archive: []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n", "dist": "not a dir\n"}},
			want:    ArchiveComparison{FirstDifference: "dist/index.js", Missing: true},
		},
		{
			name:    "verify when the runner kept a symlink with the same target then they are identical",
			archive: []tarEntry{topDir, actionYML, fileLink},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n"}, links: map[string]string{"link.yml": "action.yml"}},
			want:    ArchiveComparison{Identical: true},
		},
		{
			// Linux and macOS runners copy a symlink as the bytes of its target (F5).
			name:    "verify when the runner holds the link target's bytes then they are identical",
			archive: []tarEntry{topDir, actionYML, fileLink},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n", "link.yml": "name: curated\n"}},
			want:    ArchiveComparison{Identical: true},
		},
		{
			// Windows runners unzip a symlink as a file holding the link path (F5).
			name:        "verify when a Windows runner holds the link path as text then they are identical",
			archive:     []tarEntry{topDir, actionYML, fileLink},
			runner:      runnerLayout{files: map[string]string{"action.yml": "name: curated\n", "link.yml": "action.yml"}},
			linksAsText: true,
			want:        ArchiveComparison{Identical: true},
		},
		{
			// A link target is any string, code included; only Windows runners write it as the file.
			name:    "verify when a non-Windows runner holds the link path as text then it differs",
			archive: []tarEntry{topDir, {name: compareTop + "index.js", linkname: "require('x').run()"}},
			runner:  runnerLayout{files: map[string]string{"index.js": "require('x').run()"}},
			want:    ArchiveComparison{FirstDifference: "index.js"},
		},
		{
			// The runner's target is not in the archive, so matching it proves nothing: a moved tag
			// can ship the same bytes at both paths.
			name:    "verify when a file symlink's target is not in the archive then it differs",
			archive: []tarEntry{topDir, distDir, {name: compareTop + "dist/index.js", linkname: "../build/index.js"}},
			runner:  runnerLayout{files: map[string]string{"dist/index.js": "evil();\n", "build/index.js": "evil();\n"}},
			want:    ArchiveComparison{FirstDifference: "dist/index.js"},
		},
		{
			name:    "verify when a directory symlink's target is not in the archive then it differs",
			archive: []tarEntry{topDir, dirLink},
			runner:  runnerLayout{files: map[string]string{"dist/index.js": "evil();\n", "lib/index.js": "evil();\n"}},
			want:    ArchiveComparison{FirstDifference: "dist"},
		},
		{
			name:    "verify when the runner holds other bytes at a symlink then it differs",
			archive: []tarEntry{topDir, actionYML, fileLink},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: curated\n", "link.yml": "name: moved!\n"}},
			want:    ArchiveComparison{FirstDifference: "link.yml"},
		},
		{
			name:    "verify when the runner copied a directory symlink as a tree equal to its target then they are identical",
			archive: []tarEntry{topDir, dirLink, libDir, libJS},
			runner:  runnerLayout{files: map[string]string{"lib/index.js": "lib();\n", "dist/index.js": "lib();\n"}},
			want:    ArchiveComparison{Identical: true},
		},
		{
			// The copied tree is what the action runs, and no archive entry names its files.
			name:    "verify when the runner's copy of a directory symlink differs from its target then it differs",
			archive: []tarEntry{topDir, dirLink, libDir, libJS},
			runner:  runnerLayout{files: map[string]string{"lib/index.js": "lib();\n", "dist/index.js": "evil();\n"}},
			want:    ArchiveComparison{FirstDifference: "dist"},
		},
		{
			name:    "verify when the runner's copy of a directory symlink has an extra file then it differs",
			archive: []tarEntry{topDir, dirLink, libDir, libJS},
			runner:  runnerLayout{files: map[string]string{"lib/index.js": "lib();\n", "dist/index.js": "lib();\n", "dist/extra.js": "x\n"}},
			want:    ArchiveComparison{FirstDifference: "dist"},
		},
		{
			// A target outside the archive is never followed, so only the link text can match it.
			name:    "verify when a symlink escapes the root then its target is not followed",
			archive: []tarEntry{topDir, {name: compareTop + "secret", linkname: "../../outside"}},
			runner:  runnerLayout{files: map[string]string{"secret": "copied bytes\n"}},
			want:    ArchiveComparison{FirstDifference: "secret"},
		},
		{
			name:    "verify when the pax header holds a full SHA then it is reported",
			archive: []tarEntry{{pax: map[string]string{"comment": branchTip}}, topDir, actionYML},
			runner:  runnerLayout{files: matching},
			want:    ArchiveComparison{Identical: true, PaxSHA: branchTip},
		},
		{
			name:    "verify when the pax header comment is not an object id then no SHA is reported",
			archive: []tarEntry{{pax: map[string]string{"comment": "not-a-sha"}}, topDir, actionYML},
			runner:  runnerLayout{files: matching},
			want:    ArchiveComparison{Identical: true},
		},
		{
			// GitHub writes the header first, so it is read before the comparison stops early.
			name:    "verify when the first file differs then the pax SHA is still reported",
			archive: []tarEntry{{pax: map[string]string{"comment": branchTip}}, topDir, actionYML},
			runner:  runnerLayout{files: map[string]string{"action.yml": "name: moved\n"}},
			want:    ArchiveComparison{FirstDifference: "action.yml", PaxSHA: branchTip},
		},
		{
			name:    "verify when an entry climbs out of the top directory then it errors",
			archive: []tarEntry{topDir, actionYML, {name: compareTop + "../evil.txt", content: "x"}},
			runner:  runnerLayout{files: matching},
			wantErr: true,
		},
		{
			name:    "verify when an entry is absolute below the top directory then it errors",
			archive: []tarEntry{topDir, {name: compareTop + "/etc/passwd", content: "x"}},
			runner:  runnerLayout{files: matching},
			wantErr: true,
		},
		{
			name:    "verify when an entry name is absolute then it errors",
			archive: []tarEntry{{name: "/etc/passwd", content: "x"}},
			runner:  runnerLayout{files: map[string]string{"etc/passwd": "x"}},
			wantErr: true,
		},
		{
			// A backslash is a separator only on Windows, where it must not climb out of the root.
			name:    "verify when an entry climbs out with backslashes then it errors on Windows",
			archive: []tarEntry{topDir, actionYML, {name: compareTop + `..\..\evil.txt`, content: "x"}},
			runner:  runnerLayout{files: matching},
			want:    ArchiveComparison{FirstDifference: `..\..\evil.txt`, Missing: true},
			wantErr: runtime.GOOS == "windows",
		},
		{
			// Comparing nothing must not approve anything.
			name:    "verify when the archive holds only its top directory then it errors",
			archive: []tarEntry{{pax: map[string]string{"comment": branchTip}}, topDir},
			runner:  runnerLayout{files: matching},
			wantErr: true,
		},
		{
			name:    "verify when a file sits beside the top directory then it errors",
			archive: []tarEntry{topDir, actionYML, {name: "README.md", content: "x"}},
			runner:  runnerLayout{files: matching},
			wantErr: true,
		},
		{
			name:    "verify when the archive has a second top directory then it errors",
			archive: []tarEntry{topDir, actionYML, {name: "other/action.yml", content: "name: curated\n"}},
			runner:  runnerLayout{files: matching},
			wantErr: true,
		},
		{
			name:       "verify when the archive is not gzip then it errors",
			rawArchive: []byte(`{"errors":[{"status":500}]}`),
			runner:     runnerLayout{files: matching},
			wantErr:    true,
		},
		{
			// Self-hosted runners with ACTIONS_RUNNER_SYMLINK_CACHED_ACTIONS link the action into a shared cache (F13).
			name:          "verify when the action directory is a symlink then its target is compared",
			archive:       []tarEntry{topDir, actionYML, distDir, indexJS},
			runner:        runnerLayout{files: matching},
			rootIsSymlink: true,
			want:          ArchiveComparison{Identical: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.rawArchive
			if raw == nil {
				raw = buildTarGz(t, tt.archive...)
			}
			original := linksArriveAsText
			linksArriveAsText = tt.linksAsText
			t.Cleanup(func() { linksArriveAsText = original })
			archivePath := writeTestArchive(t, raw)
			runnerDir := tt.runner.build(t)
			if tt.rootIsSymlink {
				link := filepath.Join(t.TempDir(), "v4")
				require.NoError(t, os.Symlink(runnerDir, link))
				runnerDir = link
			}

			got, err := CompareArchive(archivePath, runnerDir)

			if tt.wantErr {
				assert.Error(t, err, "CompareArchive() = %+v, want an error", got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
