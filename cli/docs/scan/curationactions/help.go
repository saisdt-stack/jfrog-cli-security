package curationactions

func GetDescription() string {
	return "Curate the third-party GitHub Actions resolved on this job's runner."
}

func GetAIDescription() string {
	return `Curate every GitHub Action the runner downloaded for this job, including actions pulled in transitively through another action, and report Approved, Rejected or Undetermined per action against the curation policies of the Artifactory repository governing the job's GitHub repository. Each action is checked against those policies and the runner's copy is compared with the version Artifactory approved: a policy block or a content mismatch is Rejected, with the reason in Notes. The job fails unless every action is Approved. Use when a GitHub Actions job must not run third-party action code that curation has not approved.

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
- As a workflow step, put it first in the job. Even then, the pre scripts of the job's actions ('pre:' in their action.yml) run before it; only the hook runs before them.
- --threads sets how many actions are decided at once (default 3). Given with --install-runner-hook, the hook uses that value.
- An action that cannot be decided (Artifactory unreachable, ref not found) is reported Undetermined and fails the job; the other actions are still decided. An authentication failure stops the run.
- A tag or branch that moved after the runner downloaded it can be Rejected as a content mismatch; re-running the job resolves it. Pinning actions to a full commit SHA avoids this.
- After installing the hook, start the runner, or restart it if it is already running. The hook runs before any step or pre script, decides each action by the exact commit the runner fetched, so the report and the curation audit show that SHA, and its report appears in the job's "Set up runner" step and on the run's summary page.
- Install is refused inside a GitHub Actions job, and for a runner directory whose path the runner cannot start a hook from (a quote on Windows, whitespace elsewhere).
- A runner runs only one job-started hook. If one is already set (in the runner's .env, or in the environment install runs in), install leaves it in place, writes the check's script, and prints the line to add as the first command of that hook; run install again afterwards to confirm. A hook set only in the runner service's own environment (a systemd unit, container image or pod spec) cannot be seen by install and is replaced by the .env setting it writes - call the check from that hook instead, as install's warning explains. Uninstall is refused while such a hook still calls the check.
- Ephemeral and autoscaled runners (--ephemeral, Actions Runner Controller, VM or container images): a hook installed on a running runner is lost when that runner is replaced, so make the install part of how the runner is built. Install needs a configured runner directory, so run it after the runner's config step and before it starts: in the image build when the image is configured there, or in the start script when the runner is configured at start (Actions Runner Controller and just-in-time configuration).
- Not curated: 'uses: docker://<image>' steps and the images of container actions (curate those with 'jf curation-audit --image <image>'); actions fetched by a 'run:' step; and actions reached only through a local composite action ('uses: ./...'), which the runner resolves after the check runs - the report notes this limit.
- A called reusable workflow's jobs run as separate jobs: a step in the calling job does not curate them, so add a jf curate-gh-actions step inside the reusable workflow. On a self-hosted runner with the hook installed, each of those jobs is curated by the hook.
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

Q: Does this curate a step that uses docker://<image>?
A: No. Curate the image with jf curation-audit --image <image>.
`
}
