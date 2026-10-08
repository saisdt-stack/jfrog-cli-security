package curationactions

func GetDescription() string {
	return "Curate the third-party GitHub Actions resolved on this job's runner."
}

func GetAIDescription() string {
	return `Curate every GitHub Action the runner downloaded for a job, including actions pulled in transitively through another action, and report Approved, Rejected or Undetermined per action against the curation policies of the Artifactory repository governing the job's GitHub repository. A Rejected action has the reason in Notes. The job fails unless every action is Approved. Use when a GitHub Actions job must not run third-party action code that curation has not approved.

How to set it up:
- GitHub-hosted runners: add jfrog/setup-jfrog-cli with curation-enabled: true as the first step of every job; the action runs this check for you. Do not call jf curate-gh-actions directly anywhere in a workflow.
- Self-hosted runners (recommended): the runner admin installs this command as the runner's job-started hook with jf curate-gh-actions --install-runner-hook, so every job on the runner is curated whatever its workflow contains. If the runner uses an action archive cache (ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE) that only administrators can write, add --trust-action-cache to the install command.
- Self-hosted runners without the hook: workflows can opt in the same way as on GitHub-hosted runners, with jfrog/setup-jfrog-cli and curation-enabled: true as the first step of every job. Only the jobs that do so are curated, so the hook remains the recommended setup.

Prerequisites:
- A GitHub Actions runner; elsewhere it reports an error.
- Least-privilege credentials whose user or group has read permission only on the GitHub Actions VCS remote repository. With jfrog/setup-jfrog-cli, configure it with OIDC, with the identity mapping limited that way. For the hook, use a dedicated access token, not a username and password, with an expiry, and rotate it.
- A self-signed or internal-CA Artifactory needs its CA certificate in the runner's trust store or ~/.jfrog/security/certs; TLS verification cannot be turned off.
- For the hook: the runner's installation directory, and a dedicated JFrog CLI home. Configure the token in a folder that holds only that token - not your own ~/.jfrog - and run install with JFROG_CLI_HOME_DIR set to it; pick the server with --server-id. The hook script, the jf binary and that folder must be writable by the admin only and readable by the runner's service account (on Windows the default NETWORK SERVICE account usually cannot read a user profile). An expired or revoked token fails every job on the runner until it is replaced. On a Windows desktop without PowerShell 7, allow local scripts with an execution policy such as RemoteSigned.

Common patterns (self-hosted runner admin):
  $ JFROG_CLI_HOME_DIR=/opt/jfrog-curation jf curate-gh-actions --install-runner-hook --runner-dir /opt/actions-runner --server-id prod
  $ JFROG_CLI_HOME_DIR=/opt/jfrog-curation jf curate-gh-actions --install-runner-hook --runner-dir /opt/actions-runner --server-id prod --threads 8 --trust-action-cache
  > jf curate-gh-actions --install-runner-hook --runner-dir C:\actions-runner --server-id prod
  $ jf curate-gh-actions --uninstall-runner-hook --runner-dir /opt/actions-runner

Gotchas:
- After installing the hook, start the runner, or restart it if it is already running. The hook's report appears in the job's "Set up runner" step and on the run's summary page.
- --threads sets how many actions are decided at once (default 3); given with --install-runner-hook, the hook uses that value.
- Give --trust-action-cache only when administrators alone can write the runner's action archive cache. Without it, actions loaded from the cache are still curated.
- Prefer ephemeral self-hosted runners where curation enforcement matters.
- An action that cannot be decided (Artifactory unreachable, ref not found) is reported Undetermined and fails the job; the other actions are still decided. An authentication failure stops the run.
- A Rejected or Undetermined action is prevented from running; continue-on-error does not change that.
- A tag or branch that moved after the runner downloaded it can be Rejected as a content mismatch; re-running the job resolves it. Pinning actions to a full commit SHA avoids this.
- Install is refused inside a GitHub Actions job, and for a runner directory whose path the runner cannot start a hook from (a quote on Windows, whitespace elsewhere).
- A runner runs only one job-started hook. If one is already set (in the runner's .env, or in the environment install runs in), install leaves it in place, writes the check's script, and prints the line to add as the first command of that hook; run install again afterwards to confirm. If the runner's hook is set in the runner service's own environment (a systemd unit, container image or pod spec), call the check from that hook instead of using install. Uninstall is refused while another hook still calls the check.
- Ephemeral and autoscaled runners (--ephemeral, Actions Runner Controller, VM or container images): a hook installed on a running runner is lost when that runner is replaced, so make the install part of how the runner is built. Install needs a configured runner directory, so run it after the runner's config step and before it starts: in the image build when the image is configured there, or in the start script when the runner is configured at start (Actions Runner Controller and just-in-time configuration).
- A called reusable workflow's jobs run as separate jobs. Without the hook, add jfrog/setup-jfrog-cli with curation-enabled: true as the first step of each job in the reusable workflow; on a self-hosted runner with the hook installed, those jobs are curated by the hook.
- Not curated: local actions ('uses: ./...') and the actions they reference, 'uses: docker://<image>' steps and the images of container actions (curate those with 'jf curation-audit --image <image>').
- The Parent column, naming the composite action that pulled an action in, is best-effort and may be omitted; it never changes what is curated.

Related: jf curation-audit

QA:
Q: How do I curate the GitHub Actions my jobs use on GitHub-hosted runners?
A: Add jfrog/setup-jfrog-cli with curation-enabled: true as the first step of every job. Do not call jf curate-gh-actions directly in the workflow.

Q: How do I make sure every job on my self-hosted runner is curated, before any action code runs?
A: Install it as the runner's job-started hook: JFROG_CLI_HOME_DIR=<dedicated folder> jf curate-gh-actions --install-runner-hook --runner-dir <runner directory> --server-id <server>, then restart the runner. For ephemeral or autoscaled runners, run the install after the runner is configured and before it starts, in the image build or the start script.

Q: Can I use jfrog/setup-jfrog-cli on a self-hosted runner instead of the hook?
A: Yes. Add it with curation-enabled: true as the first step of every job, as on GitHub-hosted runners. Only the jobs that do so are curated, so the hook is the recommended setup for self-hosted runners.

Q: My self-hosted runner uses an action archive cache. What should I add?
A: If only administrators can write the cache (ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE), add --trust-action-cache to the install command. Otherwise leave it out; the cached actions are still curated.

Q: How do I make the hook curate faster?
A: Install it with a higher --threads, e.g. --threads 8. The default is 3.

Q: Which credentials should the hook use?
A: A dedicated, expiring access token that can read only the GitHub Actions VCS remote repository, in a JFrog CLI home of its own: run install with JFROG_CLI_HOME_DIR set to a folder holding only that token, and rotate it.

Q: My runner already has a job-started hook. Can I still install the check?
A: Yes. The runner runs only one hook, so install keeps yours and prints the line to add as its first command; run install again to confirm.

Q: Does this curate the actions used by a reusable workflow my job calls?
A: Not from the calling job. Without the hook, add jfrog/setup-jfrog-cli with curation-enabled: true as the first step of each job in that reusable workflow. With the hook installed on a self-hosted runner, its jobs are curated by the hook.

Q: Why does an all-Approved report still mention local composite actions?
A: Local composite actions are not covered by this check, so the report lists them for you to review.

Q: What does 'verified by content' in Notes mean?
A: The action was verified by an alternative method. The verdict is just as authoritative, and no action is needed.

Q: Does this curate a step that uses docker://<image>?
A: No. Curate the image with jf curation-audit --image <image>.
`
}
