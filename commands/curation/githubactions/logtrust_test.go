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
		{name: "hook with consistent logs is trusted", mode: ModeHook, wantTrusted: true},
		{
			name:        "hook without a readable job message is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Job, s.JobParsed = JobFacts{}, false },
			wantJobText: "job message could not be read",
		},
		{
			name:        "hook with a job message that names no steps is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Job.Steps = nil },
			wantJobText: "job message could not be read",
		},
		{
			name:        "hook with a planted Worker log is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Planted = true },
			wantJobText: "Worker log other than the job's own",
		},
		{
			name:        "hook with a setup buffer that may hold other jobs' lines is not trusted",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupBufferUnfiltered = true },
			wantJobText: "setup buffer",
		},
		{name: "pre of the first step is trusted, whatever the name's case", mode: ModePre, self: "Actions/Checkout", gitHubHosted: true, wantTrusted: true},
		{
			name: "pre of the second step is not trusted",
			mode: ModePre,
			self: "octo/tool",
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("repository", "GitHub", "octo/tool", "v1"))
				s.Worker = append(s.Worker, worker("octo", "tool", shaNode, SourceSave))
			},
			wantJobText: "first step",
		},
		{
			name:        "pre named like a local first step is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.Steps[0].RepositoryType = "self" },
			wantJobText: "first step",
		},
		{
			name: "pre of an action used again later in the job is not trusted",
			mode: ModePre,
			self: "actions/checkout",
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("script", "", "", ""), step("repository", "github", "Actions/Checkout", "v3"))
			},
			wantJobText: "more than once",
		},
		{
			name:        "pre with no steps read is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.Steps = nil },
			wantJobText: "cannot be proven",
		},
		{name: "pre without its own name is not trusted", mode: ModePre, wantJobText: "cannot be proven"},
		{
			name:        "pre after service containers is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.HasServices = true },
			wantJobText: "service, a container or an image build",
		},
		{
			name:        "pre in a job container is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.Job.HasContainer = true },
			wantJobText: "service, a container or an image build",
		},
		{
			name:        "pre after a Dockerfile image build is not trusted",
			mode:        ModePre,
			self:        "actions/checkout",
			edit:        func(s *DiagSnapshot) { s.BuildsImage = true },
			wantJobText: "service, a container or an image build",
		},
		{
			name: "pre stays trusted with a docker pull step and a continue-on-error step",
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
		{name: "unknown caller mode is never trusted", self: "actions/checkout", wantJobText: "position in the job is unknown"},
		{
			name:        "stale setup line for a step's action downgrades every commit logged for that repository",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupJob = append(s.SetupJob, setup("actions", "checkout", "v3", shaV3)) },
			wantTrusted: true,
			wantReasons: map[string]string{checkoutV4: ReasonStaleLine, AssessmentKey("actions", "checkout", ""): ReasonStaleLine},
		},
		{
			name: "forged setup line downgrades the commit the Worker logged under another ref",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("repository", "GitHub", "x/y", "v9"))
				s.SetupJob = append(s.SetupJob, setup("x", "y", "v9", shaV3))
				s.Worker = append(s.Worker, worker("x", "y", shaNode, SourceSave))
			},
			wantTrusted: true,
			wantReasons: map[string]string{AssessmentKey("x", "y", shaNode): ReasonStaleLine, AssessmentKey("x", "y", ""): ReasonStaleLine},
		},
		{
			name:        "stale setup line for an action found only on disk and never logged by the Worker",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupJob = append(s.SetupJob, setup("Octo", "Inner", "main", shaNode)) },
			refs:        []ActionRef{{Owner: "octo", Repo: "inner", Ref: "main"}},
			wantTrusted: true,
			wantReasons: map[string]string{AssessmentKey("octo", "inner", ""): ReasonStaleLine},
		},
		{
			name: "stale line wins over cache source for the same commit",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Worker[0].Source = SourceCache
				s.SetupJob = append(s.SetupJob, setup("actions", "checkout", "v3", shaV3))
			},
			wantTrusted: true,
			wantReasons: map[string]string{checkoutV4: ReasonStaleLine, AssessmentKey("actions", "checkout", ""): ReasonStaleLine},
		},
		{
			name:        "setup line for an action that is not this job's is ignored",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.SetupJob = append(s.SetupJob, setup("other", "job", "v1", shaNode)) },
			wantTrusted: true,
		},
		{
			name: "repository step without a Worker entry has no SHA",
			mode: ModeHook,
			edit: func(s *DiagSnapshot) {
				s.Job.Steps = append(s.Job.Steps, step("repository", "github", "Octo/Tool", "v1"))
			},
			wantTrusted: true,
			wantReasons: map[string]string{AssessmentKey("octo", "tool", ""): ReasonNoSHA},
		},
		{
			name: "local, docker and script steps need no Worker entry",
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
			name:        "Worker entry for a remote composite's inner action needs no step",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Worker = append(s.Worker, worker("octo", "inner", shaNode, SourceSave)) },
			wantTrusted: true,
		},
		{
			name:        "cache-loaded action on a self-hosted runner without an override",
			mode:        ModeHook,
			edit:        func(s *DiagSnapshot) { s.Worker[0].Source = SourceCache },
			wantTrusted: true,
			wantReasons: map[string]string{checkoutV4: ReasonCacheSource},
		},
		{
			name: "trusting another cache entry does not cover this one",
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
			name:         "cache-loaded action from a trusted cache",
			mode:         ModeHook,
			edit:         func(s *DiagSnapshot) { s.Worker[0].Source = SourceCache },
			trustedCache: func(WorkerAction) bool { return true },
			wantTrusted:  true,
		},
		{
			name:         "cache-loaded action on a GitHub-hosted runner",
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
			if tt.wantJobText != "" {
				assert.Contains(t, strings.Join(got.JobReasons, "\n"), tt.wantJobText)
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
