package githubactions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func worker(owner, repo, sha, source string) WorkerAction {
	return WorkerAction{Owner: owner, Repo: repo, SHA: sha, Source: source}
}

func setup(owner, repo, ref, sha string) LoggedAction {
	return LoggedAction{Owner: owner, Repo: repo, Ref: ref, SHA: sha}
}

func step(typ, repoType, name, ref string) JobStep {
	return JobStep{Type: typ, RepositoryType: repoType, Name: name, Ref: ref}
}

// consistentSnapshot is a job whose first step is actions/checkout and whose setup lines, job message
// and Worker log all agree.
func consistentSnapshot() DiagSnapshot {
	return DiagSnapshot{
		SetupJob:  []LoggedAction{setup("actions", "checkout", "v4", shaV4)},
		Worker:    []WorkerAction{worker("actions", "checkout", shaV4, SourceSave)},
		Job:       JobFacts{Steps: []JobStep{step("repository", "GitHub", "actions/checkout", "v4")}, TimelineID: "timeline"},
		JobParsed: true,
	}
}

func TestAssessLogs(t *testing.T) {
	checkoutV4 := AssessmentKey("actions", "checkout", shaV4)
	tests := []struct {
		name         string
		mode         CallerMode
		self         string
		gitHubHosted bool
		edit         func(*DiagSnapshot)
		refs         []ActionRef
		trustedCache func(WorkerAction) bool
		wantTrusted  bool
		wantJobText  string
		wantReasons  map[string]string
	}{
		{name: "verify when a hook reads consistent logs then the job is trusted", mode: ModeHook, wantTrusted: true},
		{
			name:        "verify when a hook cannot read the job message then the job is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Job, s.JobParsed = JobFacts{}, false },
			wantJobText: "job message could not be read",
		},
		{
			name:        "verify when a hook's job message names no steps then the job is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Job.Steps = nil },
			wantJobText: "job message could not be read",
		},
		{
			name:        "verify when a Worker log other than the job's own is present then the job is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Planted = true },
			wantJobText: "Worker log other than the job's own",
		},
		{
			name:        "verify when the setup buffer may hold other jobs' lines then the job is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupBufferUnfiltered = true },
			wantJobText: "setup buffer",
		},
		{name: "verify when the pre is the first step, named in any case, then the job is trusted", mode: ModePre, self: "Actions/Checkout", gitHubHosted: true, wantTrusted: true},
		{
			name: "verify when the pre is the second step then the job is not trusted",
			mode: ModePre,
			self: "octo/tool",
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("repository", "GitHub", "octo/tool", "v1"))
				s.Worker = append(s.Worker, worker("octo", "tool", shaNode, SourceSave))
			},
			wantJobText: "first step",
		},
		{
			name:        "verify when the first step is a local action named like the pre then the job is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.Steps[0].RepositoryType = "self" },
			wantJobText: "first step",
		},
		{
			name: "verify when the pre's action is used again later in the job then the job is not trusted",
			mode: ModePre,
			self: "actions/checkout",
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("script", "", "", ""), step("repository", "github", "Actions/Checkout", "v3"))
			},
			wantJobText: "more than once",
		},
		{
			name:        "verify when the pre reads no steps then the job is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.Steps = nil },
			wantJobText: "cannot be proven",
		},
		{name: "verify when the pre does not know its own name then the job is not trusted", mode: ModePre, wantJobText: "cannot be proven"},
		{
			name:        "verify when the job starts service containers then the pre is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.HasServices = true },
			wantJobText: "service, a container or an image build",
		},
		{
			name:        "verify when the job runs in a container then the pre is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.HasContainer = true },
			wantJobText: "service, a container or an image build",
		},
		{
			name:        "verify when the job builds a Dockerfile image then the pre is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.BuildsImage = true },
			wantJobText: "service, a container or an image build",
		},
		{
			name: "verify when the job has a docker pull step and a continue-on-error step then the pre stays trusted",
			mode: ModePre,
			self: "actions/checkout",
			edit: func(s *DiagSnapshot) {
				pull := step("containerRegistry", "", "", "")
				lenient := step("script", "", "", "")
				lenient.ContinueOnError = true
				s.Job.Steps = append(s.Job.Steps, pull, lenient)
			},
			wantTrusted: true,
		},
		{name: "verify when the caller mode is unknown then the job is never trusted", self: "actions/checkout", wantJobText: "position in the job is unknown"},
		{
			name:        "verify when a setup line names a commit the Worker did not fetch then every commit logged for that repository is stale",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupJob = append(s.SetupJob, setup("actions", "checkout", "v3", shaV3)) },
			wantTrusted: true,
			wantReasons: map[string]string{checkoutV4: ReasonStaleLine, AssessmentKey("actions", "checkout", ""): ReasonStaleLine},
		},
		{
			name:        "verify when a stale setup line names an action found only on disk then its SHA-less key is stale",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupJob = append(s.SetupJob, setup("Octo", "Inner", "main", shaNode)) },
			refs:        []ActionRef{{Owner: "octo", Repo: "inner", Ref: "main"}},
			wantTrusted: true,
			wantReasons: map[string]string{AssessmentKey("octo", "inner", ""): ReasonStaleLine},
		},
		{
			name: "verify when a commit is both stale and cache-loaded then stale-line is the reason",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Worker[0].Source = SourceCache
				s.SetupJob = append(s.SetupJob, setup("actions", "checkout", "v3", shaV3))
			},
			wantTrusted: true,
			wantReasons: map[string]string{checkoutV4: ReasonStaleLine, AssessmentKey("actions", "checkout", ""): ReasonStaleLine},
		},
		{
			name:        "verify when a setup line names an action outside this job then it is ignored",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupJob = append(s.SetupJob, setup("other", "job", "v1", shaNode)) },
			wantTrusted: true,
		},
		{
			name: "verify when a repository step has no Worker entry then it is downgraded as no-sha",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("repository", "github", "Octo/Tool", "v1"))
			},
			wantTrusted: true,
			wantReasons: map[string]string{AssessmentKey("octo", "tool", ""): ReasonNoSHA},
		},
		{
			name: "verify when local, docker and script steps have no Worker entry then nothing is downgraded",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps,
					step("repository", "self", "", ""),
					step("containerRegistry", "", "", ""),
					step("script", "", "", ""))
			},
			wantTrusted: true,
		},
		{
			name:        "verify when the Worker logged a remote composite's inner action without a step then nothing is downgraded",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Worker = append(s.Worker, worker("octo", "inner", shaNode, SourceSave)) },
			wantTrusted: true,
		},
		{
			name:        "verify when a self-hosted runner loaded an action from an untrusted cache then it is downgraded as cache-source",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Worker[0].Source = SourceCache },
			wantTrusted: true,
			wantReasons: map[string]string{checkoutV4: ReasonCacheSource},
		},
		{
			name: "verify when another cache entry is trusted then this one is still downgraded",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Worker[0].Source = SourceCache
				s.Worker = append(s.Worker, worker("octo", "inner", shaNode, SourceCache))
			},
			trustedCache: func(w WorkerAction) bool { return w.Repo == "inner" },
			wantTrusted:  true,
			wantReasons:  map[string]string{checkoutV4: ReasonCacheSource},
		},
		{
			name:         "verify when the cache entry is trusted then the cache-loaded action is not downgraded",
			mode:         ModeHook,
			edit:         func(s *DiagSnapshot) { s.Worker[0].Source = SourceCache },
			trustedCache: func(WorkerAction) bool { return true },
			wantTrusted:  true,
		},
		{
			name:         "verify when a GitHub-hosted runner loaded an action from cache then it is not downgraded",
			mode:         ModePre,
			self:         "actions/checkout",
			gitHubHosted: true,
			edit:         func(s *DiagSnapshot) { s.Worker[0].Source = SourceCache },
			wantTrusted:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := consistentSnapshot()
			if tt.edit != nil {
				tt.edit(&snap)
			}
			got := AssessLogs(LogEvidence{
				Snapshot:     snap,
				Mode:         tt.mode,
				Self:         tt.self,
				GitHubHosted: tt.gitHubHosted,
				Refs:         tt.refs,
				TrustedCache: tt.trustedCache,
			})
			assert.Equal(t, tt.wantTrusted, got.JobTrusted, "AssessLogs().JobTrusted, reasons %q", got.JobReasons)
			assert.Equal(t, tt.wantTrusted, len(got.JobReasons) == 0, "AssessLogs().JobReasons = %q", got.JobReasons)
			if !tt.wantTrusted {
				assert.Contains(t, strings.Join(got.JobReasons, "\n"), tt.wantJobText, "AssessLogs().JobReasons")
			}
			want := tt.wantReasons
			if want == nil {
				want = map[string]string{}
			}
			assert.Equal(t, want, got.ActionReason, "AssessLogs().ActionReason")
		})
	}
}

// TestAssessLogsReasonsCannotIssueWorkflowCommands guards the job-level warning: the runner reads
// "::command::" lines on a hook's or pre's stdout, and Self comes from the environment.
func TestAssessLogsReasonsCannotIssueWorkflowCommands(t *testing.T) {
	got := AssessLogs(LogEvidence{Snapshot: consistentSnapshot(), Mode: ModePre, Self: "evil\n::error::boom\r\x1b[2K"})
	assert.False(t, got.JobTrusted, "AssessLogs().JobTrusted")
	assert.Contains(t, strings.Join(got.JobReasons, "\n"), `"evil::error::boom[2K"`, "AssessLogs().JobReasons must name the sanitised Self")
	for _, reason := range got.JobReasons {
		assert.NotContains(t, reason, "\n")
		assert.NotContains(t, reason, "\r")
		assert.NotContains(t, reason, "\x1b")
		assert.False(t, strings.HasPrefix(reason, "::"), "reason %q starts a workflow command", reason)
	}
}
