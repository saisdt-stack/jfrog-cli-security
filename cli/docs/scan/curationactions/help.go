package curationactions

func GetDescription() string {
	return "Curate the third-party GitHub Actions resolved on this job's runner."
}

func GetAIDescription() string {
	return `Curate every GitHub Action the runner downloaded for this job, including actions pulled in transitively through another action, and report Approved, Rejected or Undetermined per action against the curation policies of the Artifactory repository governing the job's GitHub repository. Each action is checked against those policies. When the runner's logs can be trusted (the hook, or --from-pre from the first step's pre), the commit the runner logged is what is decided; otherwise the runner's copy is compared with the version Artifactory approved. A policy block or a content mismatch is Rejected, with the reason in Notes. The job fails unless every action is Approved. Use when a GitHub Actions job must not run third-party action code that curation has not approved.

When to use:
- A workflow must only run GitHub Actions that comply with the organization's curation policies.
- A self-hosted runner admin must enforce curation on every job that runs on the runner, whatever its workflow contains.

Prerequisites:
- Must run on a GitHub Actions runner; elsewhere it reports an error.
- A configured JFrog server (jf config, or jfrog/setup-jfrog-cli earlier in the job). Pick a non-default one with --server-id or JFROG_CLI_SERVER_ID; on a runner with an existing JFrog configuration, select setup-jfrog-cli's server explicitly with JFROG_CLI_SERVER_ID=setup-jfrog-cli-server (or the action's custom-server-id).
- Low-value credentials: the job can read whatever credentials the check uses - as a step they are the job's own, and the hook runs as the same user as the job's steps. The check only reads the git refs and archives of the GitHub Actions VCS remote repository, so configure an access token, not a username and password, whose user or group has read permission on that repository only, with an expiry, and rotate it. As a step, prefer jfrog/setup-jfrog-cli with OIDC, with the identity mapping limited the same way.
- A self-signed or internal-CA Artifactory needs its CA certificate in the runner's trust store or ~/.jfrog/security/certs; TLS verification cannot be turned off.
- For the hook: the runner's installation directory, and a JFrog CLI home of its own. Install pins the home it runs with, and every job on the runner can read that configuration, so run install with JFROG_CLI_HOME_DIR set to a folder that holds only the low-value token above - not your own ~/.jfrog. The hook script, the jf binary and that folder must be writable by the admin only and readable by the runner's service account (on Windows the default NETWORK SERVICE account usually cannot read a user profile). An expired or revoked token fails every job on the runner until it is replaced. On a Windows desktop without PowerShell 7, allow local scripts with an execution policy such as RemoteSigned.

Common patterns:
  $ jf curate-gh-actions
  $ jf curate-gh-actions --threads 8
  $ jf curate-gh-actions --install-runner-hook --runner-dir /opt/actions-runner --server-id prod --threads 8
  > jf curate-gh-actions --install-runner-hook --runner-dir C:\actions-runner --server-id prod
  $ jf curate-gh-actions --uninstall-runner-hook --runner-dir /opt/actions-runner

Gotchas:
- As a plain workflow step, put it first in the job. Even then, the pre scripts of the job's actions ('pre:' in their action.yml) run before it; only the hook runs before them. A step compares each runner copy with the approved archive, because it cannot know which commit the runner fetched.
- --from-pre is for a JavaScript action that wraps this command and runs it from its pre script, with that action as the first step of the job. The command then finds the runner's process from its own process tree (without PATH or other variables the workflow sets), reads the commit the runner logged for each action, and decides each action by that commit alone: the commit is requested through Artifactory, which applies the curation policies, and the archive it serves is discarded unread, not compared (the fast path). --from-pre is valid only from the first step's pre; calling it from main or post is unsupported and cannot be detected. --from-post, called from that action's post, reports the actions the runner fetched after pre and fails for one that is not Approved; it needs --from-pre to have run, and it is best-effort.
- The fast path needs logs that can be trusted. An action, or the whole job, is verified by content instead - the runner's copy is compared with the archive Artifactory approved at that commit (or at the ref when no commit was logged). This is not an error. The Notes column then ends with 'verified by content: <code>' and one warning names the reasons. Whole job: log-untrusted, when the wrapper is not provably the first step (it appears more than once, another step comes first, or the job message cannot be read), the job starts a service, a container or a Dockerfile image build before the wrapper's pre, or the runner's logs hold a planted Worker log or another job's setup buffer. Single action: no-sha, when no commit was logged for it or the runner cannot be found (a container job, for example); stale-line, when the setup lines name a commit the Worker log does not; renamed, when the commit was logged under another repository name; cache-source, when a self-hosted runner loaded the action from an archive cache that a job could have written to (see the cache note below). A curation block is Rejected without this suffix.
- A commit the runner logged that matches no folder in _actions is decided on its own and reported with the note 'unpaired logged commit'. That includes a commit of a repository with no folder at all (renamed or transferred since the workflow named it, or a log line forged through an action's docker:// image); a warning names it, and it is not an error.
- Residual risk on a persistent self-hosted runner: a job of the same workflow run that ran earlier on that runner can plant a Worker log the hook or the pre cannot tell from the job's own. Use ephemeral runners where this matters.
- --threads sets how many actions are decided at once (default 3). Given with --install-runner-hook, the hook uses that value.
- On a self-hosted runner with an action archive cache (ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE), the hook (or --from-pre) compares an action loaded from that cache by content unless the cache is owned by another user and read-only to the runner's user, since an earlier job could have changed it. On Windows the cache's permissions are not read, so such an action is always compared by content unless --trust-action-cache is given. --trust-action-cache, given with --install-runner-hook, decides such actions by their logged commit instead; it is for an administrator who keeps the cache read-only, or accepts that a job on the same user could also edit the hook.
- An action that cannot be decided (Artifactory unreachable, ref not found) is reported Undetermined and fails the job; the other actions are still decided. An authentication failure stops the run.
- A tag or branch that moved after the runner downloaded it can be Rejected as a content mismatch; re-running the job resolves it. Pinning actions to a full commit SHA avoids this.
- As the hook, or from the pre script of the action that wraps this command, a Rejected or Undetermined action's folder is removed from the runner's _actions directory (every ref folder of its repository when the commit cannot be tied to one folder; the repository folder itself stays), so its pre, main and post fail when the runner loads them instead of running; continue-on-error on the wrapping step does not change that. docker:// steps and actions the runner fetches mid-job (inside local composite actions) are outside this; the wrapper's post reports, after the fact, every action the runner fetched after its pre and fails for one that is not Approved. That post check is best-effort: the job's steps ran before it and could have written the runner's logs, and a post that cannot find or read the runner passes with a warning. When the hook or the pre reaches no verdict at all (Artifactory unreachable, or an action cache entry that cannot be read or resolved to a ref), every action ref folder is removed.
- After installing the hook, start the runner, or restart it if it is already running. The hook runs before any step or pre script, decides each action by the exact commit the runner fetched, so the report and the curation audit show that SHA, and its report appears in the job's "Set up runner" step and on the run's summary page.
- Install is refused inside a GitHub Actions job, and for a runner directory whose path the runner cannot start a hook from (a quote on Windows, whitespace elsewhere).
- A runner runs only one job-started hook. If one is already set (in the runner's .env, or in the environment install runs in), install leaves it in place, writes the check's script, and prints the line to add as the first command of that hook; run install again afterwards to confirm. A hook set only in the runner service's own environment (a systemd unit, container image or pod spec) cannot be seen by install and is replaced by the .env setting it writes - call the check from that hook instead, as install's warning explains. Uninstall is refused while such a hook still calls the check.
- Ephemeral and autoscaled runners (--ephemeral, Actions Runner Controller, VM or container images): a hook installed on a running runner is lost when that runner is replaced, so make the install part of how the runner is built. Install needs a configured runner directory, so run it after the runner's config step and before it starts: in the image build when the image is configured there, or in the start script when the runner is configured at start (Actions Runner Controller and just-in-time configuration).
- Not curated: local actions ('uses: ./...'), 'uses: docker://<image>' steps and the images of container actions (curate those with 'jf curation-audit --image <image>'); actions fetched by a 'run:' step; and actions reached only through a local composite action ('uses: ./...'), which the runner resolves after the check runs - the report notes this limit.
- A called reusable workflow's jobs run as separate jobs: a step in the calling job does not curate them, so add a jf curate-gh-actions step (or the wrapper action) inside the reusable workflow; a callee without it is not curated. On a self-hosted runner with the hook installed, each of those jobs is curated by the hook.
- The Parent column, naming the composite action that pulled an action in, is best-effort and never changes what is curated. It is omitted when the workflow file is not available (usually the case before the checkout) and in hook mode, and can be wrong inside a called reusable workflow.

Related: jf curation-audit

QA:
Q: What's the command to curate the GitHub Actions used in this job?
A: jf curate-gh-actions

Q: Can I run it outside a GitHub Actions runner?
A: No. It reads what to curate from the runner, so elsewhere it reports an error.

Q: How do I make it curate faster?
A: Raise --threads, e.g. jf curate-gh-actions --threads 8. The default is 3.

Q: How do I make sure every job on my self-hosted runner is curated, before any action code runs?
A: Install it as the runner's job-started hook: jf curate-gh-actions --install-runner-hook --runner-dir <runner directory>, then restart the runner. For ephemeral or autoscaled runners, run the install after the runner is configured and before it starts, in the image build or the start script.

Q: Can a job on my self-hosted runner read the JFrog credentials the hook uses?
A: Yes. The hook runs as the runner's service account, the same user as the job's steps, so the jf configuration it reads is readable by every job. Install with JFROG_CLI_HOME_DIR set to a folder holding only an expiring access token that can read just the GitHub Actions VCS remote repository, and rotate that token.

Q: My runner already has a job-started hook. Can I still install the check?
A: Yes. The runner runs only one hook, so install keeps yours and prints the line to add as its first command; run install again to confirm.

Q: Does this curate the actions used by a reusable workflow my job calls?
A: Not from the calling job - add a jf curate-gh-actions step inside that reusable workflow. With the hook installed on a self-hosted runner, its jobs are curated by the hook.

Q: Why does an all-Approved report still mention local composite actions?
A: The report covers what the runner had resolved when the check ran; local composite actions resolve their own references later in the job.

Q: Why was an action verified by content?
A: Its Notes end with 'verified by content: <code>'. The runner's log named a commit for it, but the check could not rely on that log, so it compared the runner's copy with the approved archive. log-untrusted: the whole job (the wrapper is not provably the first step, a service, container or image build started before it, or the runner's logs were tampered with). no-sha: no commit was logged for this action or the runner could not be found. stale-line: the setup lines and the Worker log disagree. renamed: the commit was logged under another repository name. cache-source: the action came from a writable archive cache on a self-hosted runner. The result is as strong as before, only slower. The warning in the job log gives the details.

Q: Does this curate a step that uses docker://<image>?
A: No. Curate the image with jf curation-audit --image <image>.
`
}
