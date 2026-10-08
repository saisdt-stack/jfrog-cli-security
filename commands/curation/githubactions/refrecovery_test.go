package githubactions

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const shaV2 = "0717577d45739eb3c851188b29f50ed6c0b2194e"

// cacheEntry is one _actions/<owner>/<repo>/<ref> entry: a downloaded directory with its
// .completed watermark written at completed, or a symlink into an unpacked cache at linkSHA.
type cacheEntry struct {
	ref       string
	completed time.Time
	linkSHA   string
}

func buildCache(t *testing.T, entries []cacheEntry) []ActionRef {
	t.Helper()
	root := filepath.Join(t.TempDir(), "_actions", "actions", "checkout")
	cache := t.TempDir()
	var refs []ActionRef
	for _, e := range entries {
		path := filepath.Join(root, e.ref)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		if e.linkSHA != "" {
			target := filepath.Join(cache, "actions_checkout", e.linkSHA, "checkout-"+e.linkSHA)
			require.NoError(t, os.MkdirAll(target, 0o755))
			require.NoError(t, os.Symlink(target, path))
		} else {
			require.NoError(t, os.MkdirAll(path, 0o755))
			require.NoError(t, os.WriteFile(path+watermarkSuffix, nil, 0o644))
			require.NoError(t, os.Chtimes(path+watermarkSuffix, e.completed, e.completed))
		}
		refs = append(refs, ActionRef{Owner: "actions", Repo: "checkout", Ref: e.ref, Path: path})
	}
	return refs
}

func TestAttachRunnerProvenance(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 4, 48, 0, 0, time.UTC)
	dl := func(sha string) WorkerAction {
		return WorkerAction{Owner: "actions", Repo: "checkout", SHA: sha}
	}
	tests := []struct {
		name         string
		entries      []cacheEntry
		setupJob     []LoggedAction
		worker       []WorkerAction
		wantSHAs     map[string]string // ref -> RunnerSHA; "" means left unknown
		wantUnplaced int
	}{
		{
			name:     "verify when the setup lines are present then they pair ref and SHA directly",
			entries:  []cacheEntry{{ref: "v4", completed: t0}, {ref: "v3", completed: t0.Add(time.Second)}},
			setupJob: []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}, {Owner: "actions", Repo: "checkout", Ref: "v3", SHA: shaV3}},
			worker:   []WorkerAction{dl(shaV4), dl(shaV3)},
			wantSHAs: map[string]string{"v4": shaV4, "v3": shaV3},
		},
		{
			name:     "verify when a setup line names a SHA this job's Worker log does not then it is ignored",
			entries:  []cacheEntry{{ref: "v4", completed: t0}},
			setupJob: []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV2}},
			worker:   []WorkerAction{dl(shaV4)},
			wantSHAs: map[string]string{"v4": shaV4},
		},
		{
			name:     "verify when one ref of the action exists then it takes the only SHA",
			entries:  []cacheEntry{{ref: "v4", completed: t0}},
			worker:   []WorkerAction{dl(shaV4)},
			wantSHAs: map[string]string{"v4": shaV4},
		},
		{
			name:     "verify when the ref is the SHA itself then it is matched by name",
			entries:  []cacheEntry{{ref: shaV2, completed: t0}, {ref: "v4", completed: t0.Add(time.Second)}},
			worker:   []WorkerAction{dl(shaV2), dl(shaV4)},
			wantSHAs: map[string]string{shaV2: shaV2, "v4": shaV4},
		},
		{
			name:     "verify when a ref is a symlink into the cache then its target names the SHA",
			entries:  []cacheEntry{{ref: "v4", linkSHA: shaV4}, {ref: "v3", completed: t0}},
			worker:   []WorkerAction{{Owner: "actions", Repo: "checkout", SHA: shaV4}, dl(shaV3)},
			wantSHAs: map[string]string{"v4": shaV4, "v3": shaV3},
		},
		{
			name:     "verify when several refs were downloaded then it orders downloads by watermark time",
			entries:  []cacheEntry{{ref: "v2", completed: t0.Add(225 * time.Millisecond)}, {ref: "v3", completed: t0}},
			worker:   []WorkerAction{dl(shaV3), dl(shaV2)},
			wantSHAs: map[string]string{"v3": shaV3, "v2": shaV2},
		},
		{
			name:         "verify when two watermarks have the same time then it leaves tied watermarks unpaired",
			entries:      []cacheEntry{{ref: "v2", completed: t0}, {ref: "v3", completed: t0}},
			worker:       []WorkerAction{dl(shaV3), dl(shaV2)},
			wantSHAs:     map[string]string{"v3": "", "v2": ""},
			wantUnplaced: 2,
		},
		{
			name: "verify when SHAs and refs do not pair one to one then none is guessed",
			entries: []cacheEntry{{ref: "v2", completed: t0}, {ref: "v3", completed: t0.Add(time.Second)},
				{ref: "v4", completed: t0.Add(2 * time.Second)}},
			worker:       []WorkerAction{dl(shaV3), dl(shaV2)},
			wantSHAs:     map[string]string{"v2": "", "v3": "", "v4": ""},
			wantUnplaced: 2,
		},
		{
			// The runner wipes _actions at job start, so every folder came from a download this job
			// logged: with one commit logged for the action, two refs resolving to it are both at it.
			name:     "verify when the action has one logged SHA then every ref of it is at that SHA",
			entries:  []cacheEntry{{ref: "v4", completed: t0}, {ref: "v4.4", completed: t0}},
			worker:   []WorkerAction{dl(shaV4)},
			wantSHAs: map[string]string{"v4": shaV4, "v4.4": shaV4},
		},
		{
			name:     "verify when a setup line took the action's one logged SHA then its other refs are at it too",
			entries:  []cacheEntry{{ref: "v4", completed: t0}, {ref: "v4.4", completed: t0.Add(time.Second)}},
			setupJob: []LoggedAction{{Owner: "actions", Repo: "checkout", Ref: "v4", SHA: shaV4}},
			worker:   []WorkerAction{dl(shaV4)},
			wantSHAs: map[string]string{"v4": shaV4, "v4.4": shaV4},
		},
		{
			name:     "verify when a ref is named for a commit the Worker did not log then it is not given the logged SHA",
			entries:  []cacheEntry{{ref: shaV2, completed: t0}, {ref: "v4", completed: t0.Add(time.Second)}},
			worker:   []WorkerAction{dl(shaV4)},
			wantSHAs: map[string]string{shaV2: "", "v4": shaV4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, unplaced := AttachRunnerProvenance(buildCache(t, tt.entries), tt.setupJob, tt.worker)
			require.Len(t, refs, len(tt.entries))
			got := map[string]string{}
			for _, r := range refs {
				got[r.Ref] = r.RunnerSHA
			}
			assert.Equal(t, tt.wantSHAs, got)
			assert.Len(t, unplaced, tt.wantUnplaced)
		})
	}
}

func TestAttachRunnerProvenanceReportsActionsMissingFromTheCache(t *testing.T) {
	refs := buildCache(t, []cacheEntry{{ref: "v4", completed: time.Now()}})
	worker := []WorkerAction{
		{Owner: "actions", Repo: "checkout", SHA: shaV4},
		{Owner: "actions", Repo: "setup-node", SHA: shaNode},
	}
	_, unplaced := AttachRunnerProvenance(refs, nil, worker)
	assert.Equal(t, []WorkerAction{worker[1]}, unplaced)
}

func TestAttachRunnerProvenanceMatchesARenamedRepositoryBySHA(t *testing.T) {
	// The runner names the _actions folder and the setup line as the workflow wrote the action, and
	// the Worker log by the name GitHub resolved it to, which differs after a repository transfer.
	walked := []ActionRef{{Owner: "oldowner", Repo: "tool", Ref: "v1", Path: filepath.Join(t.TempDir(), "v1")}}
	setupJob := []LoggedAction{{Owner: "oldowner", Repo: "tool", Ref: "v1", SHA: shaV4}}
	worker := []WorkerAction{{Owner: "newowner", Repo: "tool", SHA: shaV4}}
	refs, unplaced := AttachRunnerProvenance(walked, setupJob, worker)
	assert.Equal(t, shaV4, refs[0].RunnerSHA)
	assert.Empty(t, unplaced)
}

func TestRefsFromSetupJobKeepsRefsInsideTheActionCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "_actions")
	worker := []WorkerAction{{Owner: "z", Repo: "z", SHA: shaV4}}
	tests := []struct {
		name     string
		line     LoggedAction
		wantPath string // "" means the shortcut is refused
	}{
		{
			name:     "verify when the ref is an ordinary tag then the path is the action's folder",
			line:     LoggedAction{Owner: "z", Repo: "z", Ref: "release/v1", SHA: shaV4},
			wantPath: filepath.Join(cache, "z", "z", "release", "v1"),
		},
		{
			// A forged line could point the comparison at another action's folder.
			name: "verify when the ref climbs out of the action's folder then the shortcut is refused",
			line: LoggedAction{Owner: "z", Repo: "z", Ref: "../../u/u/v1", SHA: shaV4},
		},
		{
			name: "verify when the repo climbs out of its owner's folder then the shortcut is refused",
			line: LoggedAction{Owner: "z", Repo: "z/../../u/u", Ref: "v1", SHA: shaV4},
		},
		{
			name: "verify when the owner climbs out of the action cache then the shortcut is refused",
			line: LoggedAction{Owner: "..", Repo: "z", Ref: "v1", SHA: shaV4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, ok := RefsFromSetupJob([]LoggedAction{tt.line}, worker, cache)
			if tt.wantPath == "" {
				assert.False(t, ok, "RefsFromSetupJob(%+v) = %+v", tt.line, refs)
				return
			}
			require.True(t, ok)
			require.Len(t, refs, 1)
			assert.Equal(t, tt.wantPath, refs[0].Path)
		})
	}
}
