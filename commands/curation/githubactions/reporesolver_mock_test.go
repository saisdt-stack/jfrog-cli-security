package githubactions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMockArtifactoryVcsRepoResolver(t *testing.T) {
	tests := []struct {
		name       string
		override   string // VcsRepoOverrideEnvVar; "" leaves the key to derive from the owner
		githubRepo string
		want       string
		wantErr    bool
	}{
		{name: "verify when the repository is owner slash repo then the key derives from the owner", githubRepo: "my-org/my-repo", want: "my-org" + mockVcsRepoSuffix},
		{name: "verify when only the repo differs then the key is the same", githubRepo: "my-org/another-repo", want: "my-org" + mockVcsRepoSuffix},
		{name: "verify when the value has no slash then an error is returned", githubRepo: "my-org", wantErr: true},
		{name: "verify when the value is empty then an error is returned", githubRepo: "", wantErr: true},
		{name: "verify when the owner is missing then an error is returned", githubRepo: "/my-repo", wantErr: true},
		{name: "verify when the repo is missing then an error is returned", githubRepo: "my-org/", wantErr: true},
		{name: "verify when the value has an extra path segment then an error is returned", githubRepo: "my-org/my-repo/extra", wantErr: true},
		{name: "verify when the override is set then the repository resolves to it", override: "github-actions-remote", githubRepo: "other-org/x", want: "github-actions-remote"},
		{name: "verify when the override is set then a malformed repository is still rejected", override: "github-actions-remote", githubRepo: "no-slash", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(VcsRepoOverrideEnvVar, tt.override)

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
