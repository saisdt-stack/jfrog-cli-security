package githubactions

import "context"

// ActionCurationStatus is the curation outcome for one action.
type ActionCurationStatus string

const (
	ActionApproved ActionCurationStatus = "Approved"
	ActionRejected ActionCurationStatus = "Rejected"
)

// ActionCurationResult is the decision for one resolved action.
type ActionCurationResult struct {
	Status ActionCurationStatus
	Notes  string
}

// ActionCurationDecider decides the curation outcome for a single action reference. Only the
// mock implementation exists till support exists at Artifactory/Catalog.
type ActionCurationDecider interface {
	// Decide returns the curation outcome for one action reference under the policies of
	// artifactoryVcsRepo. A non-nil error means no decision was reached - distinct from
	// Rejected, and fatal to the command.
	Decide(ctx context.Context, artifactoryVcsRepo string, ref ActionRef) (ActionCurationResult, error)
}
