//go:build !linux && !darwin && !windows

package githubactions

// newInspector has no way to read the process tree here; GitHub runs no runner on these systems.
func newInspector() (inspector, error) {
	return nil, ErrRunnerNotVisible
}
