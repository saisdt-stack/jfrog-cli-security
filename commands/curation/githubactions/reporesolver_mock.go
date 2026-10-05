package githubactions

import (
	"context"
	"fmt"
	"os"
	"strings"
)

const mockVcsRepoSuffix = "-github-remote-stand-in"

// VcsRepoOverrideEnvVar, when set, names the Artifactory VCS repository every GitHub repository
// resolves to. It exists only so the command can be tested against a real Artifactory before the
// curation service's repository-mapping API exists, and goes away with this mock.
const VcsRepoOverrideEnvVar = "JFROG_CLI_CURATION_GH_ACTIONS_VCS_REPO"

// mockArtifactoryVcsRepoResolver stands in for the curation service's repository-mapping API,
// which does not exist yet.
type mockArtifactoryVcsRepoResolver struct{}

// NewMockArtifactoryVcsRepoResolver returns a resolver that derives the repository key from
// the GitHub owner.
func NewMockArtifactoryVcsRepoResolver() ArtifactoryVcsRepoResolver {
	return mockArtifactoryVcsRepoResolver{}
}

func (mockArtifactoryVcsRepoResolver) Resolve(_ context.Context, githubRepo string) (string, error) {
	owner, repo, found := strings.Cut(githubRepo, "/")
	if !found || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("github repository %q is not in <owner>/<repo> form", githubRepo)
	}
	if override := os.Getenv(VcsRepoOverrideEnvVar); override != "" {
		return override, nil
	}
	return owner + mockVcsRepoSuffix, nil
}
