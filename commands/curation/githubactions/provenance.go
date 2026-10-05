package githubactions

// ActionSource is how the runner put an action into its _actions directory. Only the runner's own
// logs say this; a step on a GitHub-hosted runner cannot tell, so there it is SourceUnknown.
type ActionSource string

const (
	SourceUnknown      ActionSource = ""
	SourceDownloaded   ActionSource = "Downloaded"
	SourceCacheSymlink ActionSource = "Cache (symlink)"
	SourceCacheArchive ActionSource = "Cache (archive)"
)
