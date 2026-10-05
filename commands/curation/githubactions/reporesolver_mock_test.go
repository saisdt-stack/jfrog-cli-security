package githubactions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMockArtifactoryVcsRepoResolver(t *testing.T) {
	t.Setenv(VcsRepoOverrideEnvVar, "")
	tests := []struct {
		name       string
		githubRepo string
		want       string
		wantErr    bool
	}{
		{"verify when the repository is owner slash repo then the key derives from the owner", "my-org/my-repo", "my-org" + mockVcsRepoSuffix, false},
		{"verify when the owner differs then the key differs", "other-org/my-repo", "other-org" + mockVcsRepoSuffix, false},
		{"verify when only the repo differs then the key is the same", "my-org/another-repo", "my-org" + mockVcsRepoSuffix, false},
		{"verify when the value has no slash then an error is returned", "my-org", "", true},
		{"verify when the value is empty then an error is returned", "", "", true},
		{"verify when the owner is missing then an error is returned", "/my-repo", "", true},
		{"verify when the repo is missing then an error is returned", "my-org/", "", true},
		{"verify when the value has an extra path segment then an error is returned", "my-org/my-repo/extra", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewMockArtifactoryVcsRepoResolver().Resolve(context.Background(), tt.githubRepo)
			if tt.wantErr {
				assert.Error(t, err, "an unmappable repository must not resolve to a default")
				assert.Empty(t, got)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestMockArtifactoryVcsRepoResolverOverride(t *testing.T) {
	t.Run("verify when the override is set then every repository resolves to it", func(t *testing.T) {
		t.Setenv(VcsRepoOverrideEnvVar, "github-actions-remote")
		for _, githubRepo := range []string{"my-org/my-repo", "other-org/x"} {
			got, err := NewMockArtifactoryVcsRepoResolver().Resolve(context.Background(), githubRepo)
			assert.NoError(t, err)
			assert.Equal(t, "github-actions-remote", got)
		}
	})
	t.Run("verify when the override is set then a malformed repository is still rejected", func(t *testing.T) {
		t.Setenv(VcsRepoOverrideEnvVar, "github-actions-remote")
		_, err := NewMockArtifactoryVcsRepoResolver().Resolve(context.Background(), "no-slash")
		assert.Error(t, err)
	})
	t.Run("verify when the override is empty then the key derives from the owner", func(t *testing.T) {
		t.Setenv(VcsRepoOverrideEnvVar, "")
		got, err := NewMockArtifactoryVcsRepoResolver().Resolve(context.Background(), "my-org/my-repo")
		assert.NoError(t, err)
		assert.Equal(t, "my-org"+mockVcsRepoSuffix, got)
	})
}
