package githubactions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const vcsAPIPath = "/artifactory/api/vcs/"

// fakeArtifactory serves getRefs and the download APIs, recording each request's escaped path.
type fakeArtifactory struct {
	refsStatus     int
	refsBody       string
	downloadStatus int
	downloadHeader http.Header
	downloadBody   []byte

	mu       sync.Mutex
	requests []string
}

func (f *fakeArtifactory) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, strings.TrimPrefix(r.URL.EscapedPath(), vcsAPIPath))
	f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, vcsAPIPath+"refs/") {
		w.WriteHeader(f.refsStatus)
		_, _ = w.Write([]byte(f.refsBody))
		return
	}
	for k, v := range f.downloadHeader {
		w.Header()[k] = v
	}
	w.WriteHeader(f.downloadStatus)
	_, _ = w.Write(f.downloadBody)
}

func (f *fakeArtifactory) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// deciderAdvertisement advertises a tag, a branch, and the measured tag/branch name collision.
var deciderAdvertisement = advertisement(
	pkt(branchTip+" HEAD\x00symref=HEAD:refs/heads/main object-format=sha1\n"),
	pkt(branchTip+" refs/heads/main\n"),
	pkt(branchTip+" refs/heads/collision\n"),
	pkt(branchTip+" refs/heads/Feature/X\n"),
	pkt("11d5960a326750d5838078e36cf38b85af677262 refs/tags/v4\n"),
	pkt(tagObject+" refs/tags/collision\n"),
	pkt(tagCommit+" refs/tags/collision^{}\n"),
)

func newTestDecider(t *testing.T, fake *fakeArtifactory) ActionCurationDecider {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	decider, err := NewArtifactoryActionCurationDecider(&config.ServerDetails{
		ArtifactoryUrl: server.URL + "/artifactory/",
		AccessToken:    testAccessToken,
	})
	require.NoError(t, err)
	return decider
}

// approvingArtifactory serves the advertisement and approves every download with filename.
func approvingArtifactory(t *testing.T, filename string) *fakeArtifactory {
	t.Helper()
	header := http.Header{}
	if filename != "" {
		header.Set(headerArtifactoryFilename, filename)
	}
	return &fakeArtifactory{
		refsStatus: http.StatusOK, refsBody: deciderAdvertisement,
		downloadStatus: http.StatusOK, downloadHeader: header, downloadBody: actionArchive(t),
	}
}

// isolateTempDir points the OS temp dir at a fresh directory and returns it, so a test can prove
// Decide leaves no spooled archive behind.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	return dir
}

func assertNoSpoolLeft(t *testing.T, tempDir string) {
	t.Helper()
	entries, err := os.ReadDir(tempDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "Decide left its spooled archive behind")
}

func TestArtifactoryDeciderApproves(t *testing.T) {
	const checkoutV4Commit = "11d5960a326750d5838078e36cf38b85af677262"
	tests := []struct {
		name         string
		owner, repo  string
		ref          string
		filename     string
		wantRequests []string
		wantNotes    string
	}{
		{
			name: "verify when a bare tag is approved then it downloads by tag and notes no SHA", owner: "actions", repo: "checkout", ref: "v4",
			filename:     "checkout-v4.tar.gz",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadTag/github-vcs/actions/checkout/v4"},
		},
		{
			name: "verify when a branch is approved then it downloads by branch and notes the resolved SHA", owner: "actions", repo: "checkout", ref: "main",
			filename:     "checkout-main-" + branchTip + ".tar.gz",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadBranch/github-vcs/actions/checkout/main"},
			wantNotes:    resolvedSHANotePrefix + branchTip,
		},
		{
			name: "verify when a full SHA is approved then git refs are not fetched", owner: "actions", repo: "checkout", ref: checkoutV4Commit,
			filename:     "checkout-" + checkoutV4Commit + ".tar.gz",
			wantRequests: []string{"downloadCommit/github-vcs/actions/checkout/" + checkoutV4Commit},
			wantNotes:    resolvedSHANotePrefix + checkoutV4Commit,
		},
		{
			name: "verify when an uppercase SHA is approved then it is requested lower-cased", owner: "actions", repo: "checkout", ref: strings.ToUpper(checkoutV4Commit),
			filename:     "checkout-" + checkoutV4Commit + ".tar.gz",
			wantRequests: []string{"downloadCommit/github-vcs/actions/checkout/" + checkoutV4Commit},
			wantNotes:    resolvedSHANotePrefix + checkoutV4Commit,
		},
		{
			name: "verify when a fully-qualified tag is approved then the short name is requested", owner: "actions", repo: "checkout", ref: "refs/tags/v4",
			filename:     "checkout-v4.tar.gz",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadTag/github-vcs/actions/checkout/v4"},
		},
		{
			name: "verify when a fully-qualified branch collides with a tag then its tip commit is requested", owner: "actions", repo: "checkout", ref: "refs/heads/collision",
			filename:     "checkout-" + branchTip + ".tar.gz",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadCommit/github-vcs/actions/checkout/" + branchTip},
			wantNotes:    resolvedSHANotePrefix + branchTip,
		},
		{
			name: "verify when owner and repo are mixed case then they are requested lower-cased", owner: "Actions", repo: "Checkout", ref: "v4",
			filename:     "checkout-v4.tar.gz",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadTag/github-vcs/actions/checkout/v4"},
		},
		{
			name: "verify when a branch name is mixed case then its casing is kept", owner: "actions", repo: "checkout", ref: "Feature/X",
			filename:     "X-" + branchTip + ".tar.gz",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadBranch/github-vcs/actions/checkout/Feature/X"},
			wantNotes:    resolvedSHANotePrefix + branchTip,
		},
		{
			name: "verify when the response names no archive then the action is approved without a note", owner: "actions", repo: "checkout", ref: "main",
			wantRequests: []string{"refs/github-vcs/actions/checkout", "downloadBranch/github-vcs/actions/checkout/main"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := isolateTempDir(t)
			fake := approvingArtifactory(t, tt.filename)
			// The runner's copy sits under its literal ref and matches the served archive.
			actionDir := runnerCache(t, tt.ref)
			before := snapshotDir(t, actionDir)
			ref := ActionRef{Owner: tt.owner, Repo: tt.repo, Ref: tt.ref, Path: actionDir}

			got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)

			require.NoError(t, err)
			assert.Equal(t, ActionCurationResult{Status: ActionApproved, Notes: tt.wantNotes}, got)
			assert.Equal(t, tt.wantRequests, fake.recorded())
			assert.Equal(t, before, snapshotDir(t, actionDir), "Decide modified the runner's copy")
			assertNoSpoolLeft(t, tempDir)
		})
	}
}

func TestArtifactoryDeciderContentMismatch(t *testing.T) {
	top := "checkout-" + branchTip + "/"
	tests := []struct {
		name          string
		ref           string
		servedArchive []tarEntry
		// runnerFiles are written into the runner's copy besides runnerCache's.
		runnerFiles map[string]string
		filename    string
		wantNotes   string
	}{
		{
			name: "verify when a served file differs from the runner's then the verdict is Rejected naming it",
			ref:  "v4",
			servedArchive: []tarEntry{
				{name: top, dir: true},
				{name: top + "action.yml", content: "name: moved\n"},
			},
			filename:  "checkout-v4.tar.gz",
			wantNotes: "not able to decide since content is mismatched (action.yml differs)",
		},
		{
			name: "verify when a served file is missing on the runner then the verdict is Rejected naming it",
			ref:  "v4",
			servedArchive: []tarEntry{
				{name: top, dir: true},
				{name: top + "action.yml", content: "name: curated\n"},
				{name: top + "dist/new.js", content: "new();\n"},
			},
			filename:  "checkout-v4.tar.gz",
			wantNotes: "not able to decide since content is mismatched (dist/new.js missing on the runner)",
		},
		{
			name: "verify when the runner holds a manifest it loads before the served one then the verdict is Rejected naming it",
			ref:  "v4",
			servedArchive: []tarEntry{
				{name: top, dir: true},
				{name: top + "action.yaml", content: "name: curated\n"},
			},
			runnerFiles: map[string]string{"action.yaml": "name: curated\n"},
			filename:    "checkout-v4.tar.gz",
			wantNotes:   "not able to decide since content is mismatched (action.yml only on the runner)",
		},
		{
			name: "verify when a mismatch has a resolved SHA then the notes carry it",
			ref:  "main",
			servedArchive: []tarEntry{
				{name: top, dir: true},
				{name: top + "action.yml", content: "name: moved\n"},
			},
			filename:  "checkout-main-" + branchTip + ".tar.gz",
			wantNotes: "not able to decide since content is mismatched (action.yml differs); resolved SHA: " + branchTip,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := isolateTempDir(t)
			fake := approvingArtifactory(t, tt.filename)
			fake.downloadBody = buildTarGz(t, tt.servedArchive...)
			actionDir := runnerCache(t, tt.ref)
			for name, content := range tt.runnerFiles {
				require.NoError(t, os.WriteFile(filepath.Join(actionDir, name), []byte(content), 0o644))
			}
			before := snapshotDir(t, actionDir)
			ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: tt.ref, Path: actionDir}

			got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)

			require.NoError(t, err, "a content mismatch is a verdict, not an error")
			assert.Equal(t, ActionCurationResult{Status: ActionRejected, Notes: tt.wantNotes}, got)
			assert.True(t, strings.HasPrefix(got.Notes, contentMismatchNote), "Notes = %q, want the searchable prefix %q", got.Notes, contentMismatchNote)
			assert.Equal(t, before, snapshotDir(t, actionDir), "Decide modified the runner's copy")
			assertNoSpoolLeft(t, tempDir)
		})
	}
}

func TestArtifactoryDeciderResolvedSHASource(t *testing.T) {
	top := "checkout-" + branchTip + "/"
	tests := []struct {
		name      string
		paxSHA    string
		filename  string
		wantNotes string
	}{
		{
			name:      "verify when only the archive's pax header has a SHA then it is noted",
			paxSHA:    tagCommit,
			filename:  "checkout-v4.tar.gz",
			wantNotes: resolvedSHANotePrefix + tagCommit,
		},
		{
			// An annotated tag's filename can name the tag object, while the header names the commit.
			name:      "verify when the filename and pax header disagree then the pax SHA is noted",
			paxSHA:    tagCommit,
			filename:  "checkout-v4-" + tagObject + ".tar.gz",
			wantNotes: resolvedSHANotePrefix + tagCommit,
		},
		{
			name:      "verify when only the filename has a SHA then it is noted",
			filename:  "checkout-v4-" + tagObject + ".tar.gz",
			wantNotes: resolvedSHANotePrefix + tagObject,
		},
		{
			name:     "verify when neither has a SHA then no note is made",
			filename: "checkout-v4.tar.gz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []tarEntry{
				{name: top, dir: true},
				{name: top + "action.yml", content: "name: curated\n"},
				{name: top + "dist/", dir: true},
				{name: top + "dist/index.js", content: "curated();\n"},
			}
			if tt.paxSHA != "" {
				entries = append([]tarEntry{{pax: map[string]string{"comment": tt.paxSHA}}}, entries...)
			}
			fake := approvingArtifactory(t, tt.filename)
			fake.downloadBody = buildTarGz(t, entries...)
			ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4", Path: runnerCache(t, "v4")}

			got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)

			require.NoError(t, err)
			assert.Equal(t, ActionCurationResult{Status: ActionApproved, Notes: tt.wantNotes}, got)
		})
	}
}

func TestArtifactoryDeciderRejects(t *testing.T) {
	t.Run("verify when a download is blocked by curation then the verdict is Rejected with the reason and the runner's copy is kept", func(t *testing.T) {
		fake := &fakeArtifactory{
			refsStatus: http.StatusOK, refsBody: deciderAdvertisement,
			downloadStatus: http.StatusForbidden, downloadBody: []byte(blockedEnvelope),
		}
		actionDir := runnerCache(t, "v4")
		before := snapshotDir(t, actionDir)
		ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4", Path: actionDir}

		got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)

		require.NoError(t, err, "a curation block is a verdict, not an error")
		assert.Equal(t, ActionCurationResult{Status: ActionRejected, Notes: "Package is blocked by policy: no-unpinned-actions"}, got)
		assert.Equal(t, before, snapshotDir(t, actionDir), "Decide modified the runner's copy")
	})
}

func TestArtifactoryDeciderRejectsNamingOnlyARequestedCommit(t *testing.T) {
	const reason = "Package is blocked by policy: no-unpinned-actions"
	tests := []struct {
		name      string
		ref       string
		wantNotes string
	}{
		{
			name:      "verify when a ref pinned to a commit is blocked then the notes name that commit",
			ref:       strings.ToUpper(tagCommit),
			wantNotes: reason + "; " + resolvedSHANotePrefix + tagCommit,
		},
		{
			name:      "verify when a branch colliding with a tag is blocked then the notes name the tip commit requested",
			ref:       "refs/heads/collision",
			wantNotes: reason + "; " + resolvedSHANotePrefix + branchTip,
		},
		{
			name:      "verify when a tag is blocked then the notes name no commit, since Artifactory resolved it",
			ref:       "v4",
			wantNotes: reason,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeArtifactory{
				refsStatus: http.StatusOK, refsBody: deciderAdvertisement,
				downloadStatus: http.StatusForbidden, downloadBody: []byte(blockedEnvelope),
			}
			ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: tt.ref, Path: t.TempDir()}

			got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)

			require.NoError(t, err)
			assert.Equal(t, ActionCurationResult{Status: ActionRejected, Notes: tt.wantNotes}, got)
		})
	}
}

func TestArtifactoryDeciderErrors(t *testing.T) {
	tests := []struct {
		name         string
		fake         *fakeArtifactory
		ref          string
		wantDenied   bool
		wantNotFound bool
		wantRequests int
	}{
		{
			name: "verify when a download returns 401 then it is an access failure",
			fake: &fakeArtifactory{refsStatus: http.StatusOK, refsBody: deciderAdvertisement, downloadStatus: http.StatusUnauthorized},
			ref:  "v4", wantDenied: true, wantRequests: 2,
		},
		{
			name: "verify when getRefs returns 401 then it is an access failure and nothing is downloaded",
			fake: &fakeArtifactory{refsStatus: http.StatusUnauthorized},
			ref:  "v4", wantDenied: true, wantRequests: 1,
		},
		{
			name: "verify when getRefs returns 403 then it is an access failure and not a Rejected verdict",
			fake: &fakeArtifactory{refsStatus: http.StatusForbidden, refsBody: blockedEnvelope},
			ref:  "v4", wantDenied: true, wantRequests: 1,
		},
		{
			name: "verify when a download returns 404 then it is an error but not an access failure",
			fake: &fakeArtifactory{refsStatus: http.StatusOK, refsBody: deciderAdvertisement, downloadStatus: http.StatusNotFound, downloadBody: []byte(notFoundBody)},
			ref:  "v4", wantRequests: 2,
		},
		{
			name: "verify when the ref is not advertised then it errors without downloading",
			fake: &fakeArtifactory{refsStatus: http.StatusOK, refsBody: deciderAdvertisement},
			ref:  "no-such-ref", wantNotFound: true, wantRequests: 1,
		},
		{
			name: "verify when the served archive is not a tarball then it errors and the runner's copy is kept",
			fake: &fakeArtifactory{refsStatus: http.StatusOK, refsBody: deciderAdvertisement, downloadStatus: http.StatusOK, downloadBody: []byte("not a tarball")},
			ref:  "v4", wantRequests: 2,
		},
		{
			// Comparing nothing must not approve the runner's copy.
			name: "verify when the served archive holds only its top directory then it errors",
			fake: &fakeArtifactory{
				refsStatus: http.StatusOK, refsBody: deciderAdvertisement, downloadStatus: http.StatusOK,
				downloadBody: buildTarGz(t, tarEntry{name: "checkout-" + branchTip + "/", dir: true}),
			},
			ref: "v4", wantRequests: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := isolateTempDir(t)
			actionDir := runnerCache(t, tt.ref)
			before := snapshotDir(t, actionDir)
			ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: tt.ref, Path: actionDir}

			got, err := newTestDecider(t, tt.fake).Decide(context.Background(), testRepoKey, ref)

			require.Error(t, err)
			assert.Equal(t, ActionCurationResult{}, got, "an error must not also carry a verdict")
			assert.Equal(t, tt.wantDenied, errors.Is(err, ErrAccessDenied), "Decide() error = %v, want errors.Is(ErrAccessDenied) = %v", err, tt.wantDenied)
			assert.Equal(t, tt.wantNotFound, errors.Is(err, ErrRefNotAdvertised), "Decide() error = %v, want errors.Is(ErrRefNotAdvertised) = %v", err, tt.wantNotFound)
			assert.Len(t, tt.fake.recorded(), tt.wantRequests)
			assert.Equal(t, before, snapshotDir(t, actionDir), "Decide modified the runner's copy")
			assertNoSpoolLeft(t, tempDir)
		})
	}
}

func TestArtifactoryDeciderHonoursCancelledContext(t *testing.T) {
	t.Run("verify when the context is already cancelled then no request is made", func(t *testing.T) {
		fake := approvingArtifactory(t, "")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := newTestDecider(t, fake).Decide(ctx, testRepoKey, ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4", Path: runnerCache(t, "v4")})

		assert.True(t, errors.Is(err, context.Canceled), "Decide() error = %v, want context.Canceled", err)
		assert.Empty(t, fake.recorded())
	})
}

func TestArtifactoryDeciderDecidesByTheRunnerSHA(t *testing.T) {
	t.Run("verify when the runner SHA is known then its commit is downloaded and the runner copy is not compared", func(t *testing.T) {
		notTheApprovedContent := t.TempDir()
		tempDir := isolateTempDir(t)
		fake := approvingArtifactory(t, "")
		ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4", Path: notTheApprovedContent, RunnerSHA: tagCommit, Verification: VerifyLoggedSHA}
		got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
		require.NoError(t, err)
		assert.Equal(t, ActionCurationResult{Status: ActionApproved, Notes: resolvedSHANotePrefix + tagCommit}, got)
		assert.Equal(t, []string{"downloadCommit/github-vcs/actions/checkout/" + tagCommit}, fake.recorded(),
			"the ref is not looked up: the runner already named the commit")
		assertNoSpoolLeft(t, tempDir)
	})
	t.Run("verify when the runner SHA is known and its commit is blocked then it is rejected with that SHA in the notes", func(t *testing.T) {
		fake := &fakeArtifactory{downloadStatus: http.StatusForbidden, downloadBody: []byte(blockedEnvelope)}
		ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: "v4", Path: t.TempDir(), RunnerSHA: tagCommit, Verification: VerifyLoggedSHA}
		got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
		require.NoError(t, err)
		assert.Equal(t, ActionCurationResult{Status: ActionRejected, Notes: "Package is blocked by policy: no-unpinned-actions; " + resolvedSHANotePrefix + tagCommit}, got)
		assert.Equal(t, []string{"downloadCommit/github-vcs/actions/checkout/" + tagCommit}, fake.recorded())
	})
}

const (
	sha40  = "0123456789abcdef0123456789abcdef01234567"
	sha40b = "89abcdef0123456789abcdef0123456789abcdef"
)

// approvingArtifactoryWith approves every download with an archive of files under the single top
// directory GitHub's archives have.
func approvingArtifactoryWith(t *testing.T, filename string, files map[string]string) *fakeArtifactory {
	t.Helper()
	top := strings.TrimSuffix(filename, ".tar.gz") + "/"
	entries := []tarEntry{{name: top, dir: true}}
	for name, content := range files {
		entries = append(entries, tarEntry{name: top + name, content: content})
	}
	fake := approvingArtifactory(t, filename)
	fake.downloadBody = buildTarGz(t, entries...)
	return fake
}

// writeTree writes files into a fresh directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	return dir
}

func TestArtifactoryDeciderLoggedSHAApprovesWithoutReadingTheRunnersCopy(t *testing.T) {
	fake := approvingArtifactory(t, "owner-repo-"+sha40+".tar.gz")
	ref := ActionRef{Owner: "Owner", Repo: "Repo", Ref: "v1", Path: filepath.Join(t.TempDir(), "absent"),
		RunnerSHA: strings.ToUpper(sha40), Verification: VerifyLoggedSHA}
	got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
	require.NoError(t, err)
	assert.Equal(t, ActionApproved, got.Status)
	assert.Equal(t, resolvedSHANotePrefix+sha40, got.Notes)
	assert.Equal(t, []string{"downloadCommit/github-vcs/owner/repo/" + sha40}, fake.recorded(), "the fast path asks for the commit and nothing else")
}

func TestArtifactoryDeciderLoggedSHAWithoutARunnerSHAComparesContent(t *testing.T) {
	fake := approvingArtifactoryWith(t, "owner-repo-"+sha40+".tar.gz", map[string]string{"action.yml": "a"})
	ref := ActionRef{Owner: "owner", Repo: "repo", Ref: "v4", Path: writeTree(t, map[string]string{"action.yml": "evil"}),
		Verification: VerifyLoggedSHA}
	got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
	require.NoError(t, err)
	assert.Equal(t, ActionRejected, got.Status)
	assert.Contains(t, got.Notes, contentMismatchNote)
}

func TestArtifactoryDeciderZeroVerificationWithARunnerSHAComparesContentAtThatCommit(t *testing.T) {
	fake := approvingArtifactoryWith(t, "owner-repo-"+sha40+".tar.gz", map[string]string{"action.yml": "a"})
	path := writeTree(t, map[string]string{"action.yml": "evil"})
	got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey,
		ActionRef{Owner: "owner", Repo: "repo", Ref: "v1", Path: path, RunnerSHA: sha40})
	require.NoError(t, err)
	assert.Equal(t, ActionRejected, got.Status)
	assert.Contains(t, got.Notes, contentMismatchNote)
	assert.Equal(t, []string{"downloadCommit/github-vcs/owner/repo/" + sha40}, fake.recorded(), "no ref resolution before the compare")
}

func TestArtifactoryDeciderBlockedActionIsNotNotedAsVerifiedByContent(t *testing.T) {
	fake := &fakeArtifactory{downloadStatus: http.StatusForbidden, downloadBody: []byte(blockedEnvelope)}
	got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ActionRef{Owner: "owner", Repo: "repo", Ref: "v1",
		Path: writeTree(t, map[string]string{"action.yml": "a"}), RunnerSHA: sha40, ContentReason: "log-untrusted"})
	require.NoError(t, err)
	assert.Equal(t, ActionRejected, got.Status)
	assert.NotContains(t, got.Notes, "verified by content", "nothing was compared, the policy blocked the download")
}

func TestArtifactoryDeciderContentAtSHANotesTheReason(t *testing.T) {
	files := map[string]string{"action.yml": "a"}
	fake := approvingArtifactoryWith(t, "owner-repo-"+sha40+".tar.gz", files)
	ref := ActionRef{Owner: "owner", Repo: "repo", Ref: "v1", Path: writeTree(t, files), RunnerSHA: sha40, ContentReason: "stale-line"}
	got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
	require.NoError(t, err)
	assert.Equal(t, ActionApproved, got.Status)
	assert.Equal(t, resolvedSHANotePrefix+sha40+"; verified by content: "+ref.ContentReason, got.Notes)
}

func TestArtifactoryDeciderContentMismatchNotesTheReason(t *testing.T) {
	fake := approvingArtifactoryWith(t, "owner-repo-"+sha40+".tar.gz", map[string]string{"action.yml": "a"})
	ref := ActionRef{Owner: "owner", Repo: "repo", Ref: "v1", Path: writeTree(t, map[string]string{"action.yml": "evil"}),
		RunnerSHA: sha40, ContentReason: "no-sha"}
	got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
	require.NoError(t, err)
	assert.Equal(t, ActionRejected, got.Status)
	assert.True(t, strings.HasSuffix(got.Notes, "; verified by content: no-sha"), got.Notes)
}

func TestArtifactoryDeciderUnpairedLoggedCommitIsDecidedBySHA(t *testing.T) {
	// A commit the Worker logged that no folder took: no ref and no path, so only its SHA can be decided.
	unpaired := ActionRef{Owner: "owner", Repo: "repo", RunnerSHA: sha40, Verification: VerifyLoggedSHA}
	tests := []struct {
		name string
		fake *fakeArtifactory
		want ActionCurationResult
	}{
		{
			name: "verify when the logged commit is approved then it is Approved and noted as unpaired",
			fake: approvingArtifactory(t, ""),
			want: ActionCurationResult{Status: ActionApproved, Notes: resolvedSHANotePrefix + sha40 + "; " + unpairedLoggedCommitNote},
		},
		{
			name: "verify when the logged commit is blocked then it is Rejected and noted as unpaired",
			fake: &fakeArtifactory{downloadStatus: http.StatusForbidden, downloadBody: []byte(blockedEnvelope)},
			want: ActionCurationResult{Status: ActionRejected,
				Notes: "Package is blocked by policy: no-unpinned-actions; " + resolvedSHANotePrefix + sha40 + "; " + unpairedLoggedCommitNote},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newTestDecider(t, tt.fake).Decide(context.Background(), testRepoKey, unpaired)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, []string{"downloadCommit/github-vcs/owner/repo/" + sha40}, tt.fake.recorded())
		})
	}
}

func TestArtifactoryDeciderLoggedSHAsPairARefWithALoggedCommit(t *testing.T) {
	// The runner's copy is absent in every paired case: nothing is read from it.
	absent := filepath.Join(t.TempDir(), "absent")
	tests := []struct {
		name   string
		ref    string
		logged []string
		want   string
	}{
		{name: "verify when an annotated tag peels to a logged commit then it is decided by that commit", ref: "collision", logged: []string{sha40, tagCommit}, want: tagCommit},
		{name: "verify when a branch tip is a logged commit then it is decided by that commit", ref: "main", logged: []string{strings.ToUpper(branchTip), sha40}, want: branchTip},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := isolateTempDir(t)
			fake := approvingArtifactory(t, "")
			ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: tt.ref, Path: absent, LoggedSHAs: tt.logged, ContentReason: "no-sha"}
			got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
			require.NoError(t, err)
			assert.Equal(t, ActionCurationResult{Status: ActionApproved, Notes: resolvedSHANotePrefix + tt.want}, got)
			assert.Equal(t, []string{"refs/github-vcs/actions/checkout", "downloadCommit/github-vcs/actions/checkout/" + tt.want}, fake.recorded())
			assertNoSpoolLeft(t, tempDir)
		})
	}
}

func TestArtifactoryDeciderLoggedSHAsComparesWhenTheRefDoesNotResolveToALoggedCommit(t *testing.T) {
	files := map[string]string{"action.yml": "a"}
	tests := []struct {
		name   string
		ref    string
		logged []string
		want   []string
	}{
		{name: "verify when a moved tag resolves to another commit then the content is compared", ref: "v4", logged: []string{sha40},
			want: []string{"refs/github-vcs/actions/checkout", "downloadTag/github-vcs/actions/checkout/v4"}},
		{name: "verify when only an annotated tag's own object is logged then it does not pair", ref: "collision", logged: []string{tagObject},
			want: []string{"refs/github-vcs/actions/checkout", "downloadTag/github-vcs/actions/checkout/collision"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := approvingArtifactoryWith(t, "checkout-"+tt.ref+".tar.gz", files)
			ref := ActionRef{Owner: "actions", Repo: "checkout", Ref: tt.ref, Path: writeTree(t, map[string]string{"action.yml": "evil"}),
				LoggedSHAs: tt.logged, ContentReason: "no-sha"}
			got, err := newTestDecider(t, fake).Decide(context.Background(), testRepoKey, ref)
			require.NoError(t, err)
			assert.Equal(t, ActionRejected, got.Status)
			assert.Contains(t, got.Notes, contentMismatchNote)
			assert.Equal(t, tt.want, fake.recorded())
		})
	}
}
