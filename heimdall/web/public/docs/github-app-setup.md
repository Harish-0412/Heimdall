# Heimdall GitHub App setup

The implementation uses its own GitHub App. The connected Codex App does not
provide installation credentials to the Heimdall runtime. A new App has not yet
been registered, and the live acceptance test remains pending that registration.

The authorized test repository is
[Harish-0412/Startup-Assisstant](https://github.com/Harish-0412/Startup-Assisstant)
(repository ID `1230542209`, default branch `main`). Install the App on this
repository alone while testing. Do not use production repositories or registry
roles for the acceptance run.

## Register the App

The ready-to-edit manifest is [config/github-app-manifest.json](../config/github-app-manifest.json).
Replace the control-plane URL and webhook URL with reachable HTTPS endpoints.
The main API webhook route is `/v1/github/webhooks` (plural). The independent
Lambda/API Gateway template below exposes `/v1/github/webhook` (singular).
Set the App manifest's webhook URL to the endpoint for the ingress you deploy.
Use a distinct App name if the suggested name is unavailable. The manifest flow
requires an operator-controlled redirect listener that validates its random
`state`, exchanges the temporary code with GitHub and saves credentials locally;
never paste the returned private key or webhook secret into chat or source code.

Manual registration is also supported and does not need that listener:

1. Open [GitHub App registration](https://github.com/settings/apps/new) while
   signed in as `Harish-0412`. Name the App `Heimdall Preview Harish` (or another
   unique name). Set its homepage to the control-plane HTTPS URL.
2. Enable webhooks and enter the webhook HTTPS URL. Generate a random webhook
   secret of at least 32 bytes using a local password manager or secure random
   generator. Save it to a local secret file or AWS Secrets Manager.
3. Set repository permissions to **Contents: read**, **Pull requests: write**,
   **Checks: write**, **Actions: read**, and the mandatory **Metadata: read**.
   Select the **Pull request** and **Issue comment** events. No organization,
   account, workflow-write, contents-write, or Actions-write scope is needed.
4. Make the App private to your account. Register it, record its App ID, and
   generate/download an RSA private key. Store the key outside the checkout in
   a directory restricted to your user/service identity. Do not commit it.
5. Install the App using **Only select repositories**, select
   `Startup-Assisstant`, and record the installation ID from the installation
   URL. The runtime's registered installation and repository IDs must match.

Installation tokens are minted for a single repository and the permission
needed by each API operation. Metadata reads check the current permission of
the commenter; `author_association` and webhook user fields are not authority.
The App never writes onboarding files. `heimdall init` creates files locally for
the repository owner to review and commit.

GitHub documents the [manifest registration flow](https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest)
and [minimum permission selection](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app).

## Wire the local runtime

Use operator provisioning to create a tenant, its customer-owned cluster and a
restricted application database connection. Register the new installation and
repository under that tenant, with the configured cluster and the **immutable
trusted reusable workflow ref and SHA**. A PR must not choose these values.

The tooling repository is `Harish-0412/Heimdall`; its reusable workflow lives at
`.github/workflows/heimdall-preview.yml` in the repository root. Publish and
review the workflow first, then use that exact commit SHA in the caller,
repository registration and AWS subject. The all-zero SHA in
[config/operator-repository.example.json](../config/operator-repository.example.json)
is a placeholder to replace, along with the installation and cluster IDs.

Build the reviewed CLI from this project's `heimdall` directory, or use a
reviewed published binary:

```powershell
go build -trimpath -o out/heimdall.exe ./cmd/heimdall
./out/heimdall.exe init --workflow 'Harish-0412/Heimdall/.github/workflows/heimdall-preview.yml@<reviewed-40-character-sha>' --out-dir '<disposable-output-directory>' --port <actual-port>
```

Replace every angle-bracket value before running this command. Review the
generated `heimdall.yaml` and caller workflow, adapt them to the application,
and copy the files into a local checkout of `Startup-Assisstant`. The starter
expects a real reviewed `Dockerfile` in its build context and the application's
actual listening port; it does not create a Dockerfile. The CLI refuses to
overwrite existing files. Review and commit the onboarding changes to `main`
before opening the two acceptance PRs.

Set these repository **Actions variables** in `Startup-Assisstant`:

| Variable | Value |
|---|---|
| `HEIMDALL_ROLE_ARN` | Customer preview-only build role ARN with the workflow-bound OIDC trust below |
| `HEIMDALL_AWS_REGION` | ECR region, for example `ap-south-1` |
| `HEIMDALL_REGISTRY` | Customer ECR preview prefix, for example `123456789012.dkr.ecr.ap-south-1.amazonaws.com/heimdall-preview` |
| `HEIMDALL_API_URL` | Reachable HTTPS API origin with no trailing slash, for example `https://preview-api.example.com` |

The workflow registry prefix has **no trailing slash**; tenant policy's
`AllowedRegistries` entry uses the same prefix **with a trailing slash**.
Pre-create `<prefix>/<service-or-worker-name>` and `<prefix>/bundle` ECR
repositories for the reviewed config. Use `HEIMDALL_API_URL` verbatim as
`HEIMDALL_OIDC_AUDIENCE` in both API and worker configuration.

Follow [control-plane.md](control-plane.md#local-startup-on-windows) to provision
the tenant, enroll the cluster, register the edited operator repository input,
and start the API plus orchestrator. Publishing the trusted workflow does not
register an App or supply App credentials.

The runtime reads the App ID and private-key file from local configuration. The
webhook ingress reads its own secret file; it does not need the App private key.
The orchestrator uses the application database role, never the migration role.
Configure the Actions OIDC audience to the API's exact audience string.

The agent makes outbound requests to the control plane and pulls images and
bundles from the customer's registry. The central API never receives fixture
bytes or cloud registry credentials. Configure an explicit allowed-registry
prefix such as `123456789012.dkr.ecr.ap-south-1.amazonaws.com/heimdall-preview/`.
The preview registry IAM role must be separate from production roles and scoped
to these preview repositories. Its OIDC trust must require the test repository
and the trusted reusable workflow; do not use wildcard repository subjects.

Configure the repository's OIDC subject customization to use exactly
`["repo", "context", "job_workflow_ref"]` with `use_default: false` through
[GitHub's repository OIDC customization endpoint](https://docs.github.com/en/rest/actions/oidc).
An administrator makes this setting independently of the PR. For a reusable
workflow pinned to `<trusted-commit-sha>`, the AWS role must require both:

```json
{
  "StringEquals": {
    "token.actions.githubusercontent.com:aud": "sts.amazonaws.com",
    "token.actions.githubusercontent.com:sub": "repo:Harish-0412/Startup-Assisstant:pull_request:job_workflow_ref:Harish-0412/Heimdall/.github/workflows/heimdall-preview.yml@<trusted-commit-sha>"
  }
}
```

Use an OIDC federated principal for `token.actions.githubusercontent.com` in the
customer AWS account and only `sts:AssumeRoleWithWebIdentity`. Grant
`ecr:GetAuthorizationToken` on `*` (the API requires that), and grant
`ecr:BatchCheckLayerAvailability`, `ecr:GetDownloadUrlForLayer`,
`ecr:BatchGetImage`, `ecr:InitiateLayerUpload`, `ecr:UploadLayerPart`,
`ecr:CompleteLayerUpload`, and `ecr:PutImage` only on the exact customer preview
repository ARNs. Pre-create those repositories through operator provisioning;
the PR build role does not need create/delete-repository or any other AWS
service permission. The agent gets a separate read-only registry role.

The Actions notification token uses the control-plane audience, while the AWS
role token uses `sts.amazonaws.com`. Both come from the same pinned reusable
job. The API checks the signed reusable workflow ref and SHA, current PR head,
repository ID, run ID and run attempt against GitHub. It supports GitHub's
default PR subject and the exact workflow-bound custom subject above; appended
or mismatched subject context is rejected. GitHub explains the
[reusable-workflow OIDC claims](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-with-reusable-workflows).

For a SQL import, an administrator must attest to the exact config hash and
sanitised fixture hash with an expiry and reason. Missing attestations fail
closed. A maintainer's `/heimdall approve <full-head-sha>` approves onboarding
when the default-branch configuration is missing; it does not attest to SQL
sanitisation. Merge `heimdall init` output to `main` before testing normal PRs.

## AWS ingress template

[infra/webhook/cloudformation.yaml](../infra/webhook/cloudformation.yaml)
provisions API Gateway HTTP ingress, a signed-webhook Lambda, an encrypted FIFO
queue, a 14-day FIFO dead letter queue and a visible-message alarm. Supply the
Linux arm64 `bootstrap` ZIP in S3, a Secrets Manager secret ARN, and optionally an
existing operator SNS topic. Keep the stack name short enough that
`<stack-name>-webhook` is at most 64 characters. A custom KMS key also needs a
specific `kms:Decrypt` grant on that key.

Build the static Linux arm64 ZIP with its required executable `bootstrap` mode,
including when packaging from Windows:

```sh
python3 -I scripts/package-webhook.py --output out/webhook.zip
```

The Lambda has only `sqs:SendMessage` on the source queue and
`secretsmanager:GetSecretValue` on its webhook secret, plus its own log-stream
permissions. It verifies the exact raw body, including API Gateway base64
requests, before decoding JSON. Accepted events use FIFO group `repositoryID#PR`.
An orchestrator consumer needs receive/delete/change-visibility on this queue.

SQS deduplication lasts five minutes. PostgreSQL's immutable delivery identity
and payload hash provide durable replay protection after that window. Monitor
the DLQ and persisted dead inbox/outbox records; fix the cause before redriving.
The [SQS FIFO delivery rules](https://docs.aws.amazon.com/AWSSimpleQueueService/latest/SQSDeveloperGuide/FIFO-queues-understanding-logic.html)
also require preserving order within a received batch; the consumer processes
one message at a time and does not bypass a failed message.

## Live acceptance procedure

Use two disposable branches and PRs in the authorized repository; their actual
numbers will be assigned by GitHub (the plan's #101 and #102 are examples).

1. Verify a signed GitHub ping/redelivery reaches the configured HTTPS ingress.
   An invalid signature must fail without inserting an inbox row.
2. Open PR A and PR B containing valid onboarding/configuration changes. Confirm
   two environment IDs, two separate preview namespaces and separate timelines.
3. Let the pinned trusted workflow deliver bundle/image digests. Confirm each
   environment reaches Ready, with one edited Heimdall comment and its own
   `Heimdall Preview` check.
4. Push to PR A, then replay an old webhook and old CI callback. Only A changes;
   its generation increases, and old status/build results cannot mark it Ready.
5. Have a non-collaborator comment `/heimdall delete` on A. Confirm a polite
   denial, a `github.command` audit record and no desired-state change. Have a
   current maintainer issue reset, retry, extend and delete in permitted states.
6. Close A. Confirm its resources are removed while B remains available. Reopen
   A, rebuild its current head and verify a new generation converges.
7. Verify a fork PR is refused before any customer preview credentials/resources
   are issued. Test an invalid config and missing-baseline approval flow.
8. Close the disposable PRs, remove their branches and confirm their namespaces
   are gone. Keep their delivery, deployment and audit history for the evidence.

Record the App/installation IDs, actual PR URLs, environment IDs, generations
and acceptance timestamps in the phase completion review. Until this procedure
runs with the new Heimdall App, report live GitHub acceptance as pending.

## Rotation and recovery

Replace the private-key file and restart the orchestrator when rotating an App
key; verify the new key, then revoke the old key in GitHub. Rotate the webhook
secret by coordinating GitHub and ingress configuration, retaining failed
deliveries for redelivery. Revoke a removed installation in tenant registration.
Credentials, source bodies and raw logs must never appear in service logs.

If the orchestrator crashes after GitHub creates a comment/check but before the
database records its ID, recovery searches a stable marker/external ID belonging
to this exact App. It refuses to adopt an author's forged marker. Retries do not
automatically re-POST ambiguous comment/check creation failures; the outbox
reconciles remote state first.
