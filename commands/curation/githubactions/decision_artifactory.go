package githubactions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jfrog/jfrog-cli-core/v2/utils/config"
	"github.com/jfrog/jfrog-client-go/utils/log"
)

const resolvedSHANotePrefix = "resolved SHA: "

// contentMismatchNote opens the Notes of an action whose runner copy is not the content Artifactory
// approved. Kept verbatim so it can be searched for.
const contentMismatchNote = "not able to decide since content is mismatched"

// spoolPattern names the temp file a downloaded archive is saved to while it is compared.
const spoolPattern = "jfrog-curation-action-*.tar.gz"

type artifactoryActionCurationDecider struct {
	client *vcsClient
}

// NewArtifactoryActionCurationDecider returns a decider that downloads each action through the
// Artifactory VCS repository - a successful download is the curation approval - and checks that the
// runner's copy is the content Artifactory served.
func NewArtifactoryActionCurationDecider(serverDetails *config.ServerDetails) (ActionCurationDecider, error) {
	client, err := newVCSClient(serverDetails, vcsHTTPRequestTimeout)
	if err != nil {
		return nil, err
	}
	return &artifactoryActionCurationDecider{client: client}, nil
}

// Decide classifies ref, downloads it from artifactoryVcsRepo, and compares the served archive with
// the runner's copy at ref.Path, which is only ever read.
//
// The runner keeps no record of the commit it downloaded, so the comparison is what proves the
// runner holds the approved version: Approved when they are identical, Rejected with
// contentMismatchNote when they are not - the runner and Artifactory resolved a moving tag or branch
// to different commits. A curation block is Rejected with Artifactory's reason. A mismatch is a
// verdict, not an error, so the other actions are still decided. When the runner's own logs name the
// commit (hook mode), decideRunnerCommit decides that commit instead and nothing is compared.
//
// Owner and Repo are matched case-insensitively. The ref is case-sensitive; only a full object ID
// is lower-cased.
func (d *artifactoryActionCurationDecider) Decide(ctx context.Context, artifactoryVcsRepo string, ref ActionRef) (ActionCurationResult, error) {
	if err := ctx.Err(); err != nil {
		return ActionCurationResult{}, err
	}
	owner, repo := strings.ToLower(ref.Owner), strings.ToLower(ref.Repo)
	if ref.RunnerSHA != "" {
		return d.decideRunnerCommit(artifactoryVcsRepo, owner, repo, ref.RunnerSHA)
	}

	var adv *RefAdvertisement
	if NeedsRefs(ref.Ref) {
		var err error
		if adv, err = d.client.GetRefs(artifactoryVcsRepo, owner, repo); err != nil {
			return ActionCurationResult{}, fmt.Errorf("reading the git refs of %s/%s: %w", owner, repo, err)
		}
	}
	resolved, err := ClassifyRef(ref.Ref, adv)
	if err != nil {
		return ActionCurationResult{}, err
	}

	body, filename, err := d.client.Download(artifactoryVcsRepo, owner, repo, resolved)
	var blocked *BlockedError
	if errors.As(err, &blocked) {
		return ActionCurationResult{Status: ActionRejected, Notes: blocked.Reason}, nil
	}
	if err != nil {
		return ActionCurationResult{}, fmt.Errorf("downloading %s %q: %w", resolved.Kind, resolved.APIRef, err)
	}
	// Spooled rather than compared from the body, so the request's timeout covers the download alone
	// and a download failure is not mistaken for a comparison failure.
	archivePath, err := spoolArchive(body)
	if err != nil {
		return ActionCurationResult{}, fmt.Errorf("downloading %s %q: %w", resolved.Kind, resolved.APIRef, err)
	}
	defer removeSpool(archivePath)

	started := time.Now()
	comparison, err := CompareArchive(archivePath, ref.Path)
	if err != nil {
		return ActionCurationResult{}, fmt.Errorf("comparing the approved %s %q with the runner's copy at %q: %w",
			resolved.Kind, resolved.APIRef, ref.Path, err)
	}
	sha := resolvedSHA(filename, comparison.PaxSHA)
	log.Debug(fmt.Sprintf("github-actions curation: %s/%s@%s via %q as %s %q, resolved SHA %q: runner copy at %q identical=%t, first difference %q, compared in %s",
		owner, repo, ref.Ref, artifactoryVcsRepo, resolved.Kind, resolved.APIRef, sha, ref.Path,
		comparison.Identical, comparison.FirstDifference, time.Since(started)))

	if !comparison.Identical {
		return ActionCurationResult{Status: ActionRejected, Notes: mismatchNotes(comparison, sha)}, nil
	}
	result := ActionCurationResult{Status: ActionApproved}
	if sha != "" {
		result.Notes = resolvedSHANotePrefix + sha
	}
	return result, nil
}

// decideRunnerCommit decides the exact commit the runner's logs say it fetched, so no ref is looked
// up and the runner's copy is not compared: the commit identifies the content, and the runner's
// action cache is the admin's, trusted to hold what its SHA names.
//
// Because the request names the commit, Artifactory's curation audit records this action by its SHA,
// not by the tag or branch the workflow wrote.
func (d *artifactoryActionCurationDecider) decideRunnerCommit(artifactoryVcsRepo, owner, repo, sha string) (ActionCurationResult, error) {
	resolved := ResolvedRef{Kind: RefKindCommit, APIRef: strings.ToLower(sha)}
	body, _, err := d.client.Download(artifactoryVcsRepo, owner, repo, resolved)
	var blocked *BlockedError
	if errors.As(err, &blocked) {
		return ActionCurationResult{Status: ActionRejected, Notes: blocked.Reason}, nil
	}
	if err != nil {
		return ActionCurationResult{}, fmt.Errorf("downloading %s %q: %w", resolved.Kind, resolved.APIRef, err)
	}
	closeResponseBody(body)
	return ActionCurationResult{Status: ActionApproved, Notes: resolvedSHANotePrefix + resolved.APIRef}, nil
}

// spoolArchive saves body to a temp file and closes both, returning the file's path.
func spoolArchive(body io.ReadCloser) (string, error) {
	defer closeResponseBody(body)
	spool, err := os.CreateTemp("", spoolPattern)
	if err != nil {
		return "", fmt.Errorf("creating a temp file for the action archive: %w", err)
	}
	_, copyErr := io.Copy(spool, body)
	// Closed before any removal: Windows cannot remove an open file.
	if err = errors.Join(copyErr, spool.Close()); err != nil {
		removeSpool(spool.Name())
		return "", fmt.Errorf("saving the action archive: %w", err)
	}
	return spool.Name(), nil
}

func removeSpool(path string) {
	if err := os.Remove(path); err != nil {
		log.Warn(fmt.Sprintf("github-actions curation: removing the downloaded action archive %q: %v", path, err))
	}
}

// resolvedSHA picks the commit to report. The archive's pax header wins: it always names the commit,
// while the filename of an annotated tag's archive can name the tag object instead.
func resolvedSHA(filename, paxSHA string) string {
	filenameSHA := ExtractResolvedSHA(filename)
	if paxSHA != "" && filenameSHA != "" && paxSHA != filenameSHA {
		log.Debug(fmt.Sprintf("github-actions curation: archive %q names %s, its pax header names %s; reporting the header's",
			filename, filenameSHA, paxSHA))
	}
	if paxSHA != "" {
		return paxSHA
	}
	return filenameSHA
}

func mismatchNotes(comparison ArchiveComparison, sha string) string {
	detail := "differs"
	if comparison.Missing {
		detail = "missing on the runner"
	}
	notes := fmt.Sprintf("%s (%s %s)", contentMismatchNote, comparison.FirstDifference, detail)
	if sha != "" {
		notes += "; " + resolvedSHANotePrefix + sha
	}
	return notes
}
