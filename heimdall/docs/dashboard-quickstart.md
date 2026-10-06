# Dashboard and your first preview

The dashboard at `/dashboard` shows environments, deployment progress,
diagnosis, timeline, short service logs and lifecycle actions. The first-preview
guide is at `/dashboard?view=setup`; documentation is at
`/dashboard?view=docs`.

P11 is being implemented. The sample dashboard is a demonstration of the
interaction design. A working live preview requires the control plane, your
own Heimdall GitHub App, a customer cluster/agent, the trusted reusable workflow
and a registry. The current release status and its gates are in
[phases.md](phases.md) and [the P5/P6 review](phase5-6-completion-review.md).

## Start the dashboard locally

From the `heimdall` directory, with Node.js 20.19+ or 22.12+ and npm:

```powershell
Set-Location web
npm install
npm run dev
```

Open `http://127.0.0.1:5173/dashboard`. Sample mode is explicitly labelled;
its environments, logs, scenario actions and allocation figures are illustrative.
They do not create namespaces, install an App or send actions to a cluster.
Use `/dashboard?view=demo` to rehearse the launch story.

For a production bundle, use `npm run build`. A static host must serve
`index.html` for dashboard deep links. Live mode additionally needs an HTTPS
same-origin reverse proxy for `/v1`. Development forwards `/v1` to the local
API on port 8080. See [web/README.md](../web/README.md).

## Connect a live API session

First bring up the API and provision a tenant using the exact procedure in
[control-plane.md](control-plane.md#local-startup-on-windows). Operator output
contains the user token in a private local file. The API requires its restricted
application database role; provisioning uses the separate operator connection.

1. Open `/dashboard?view=connections`.
2. Enter a user bearer token issued for your tenant. Do not use an agent,
   enrollment, Actions OIDC or registry token.
3. Connect and wait for the authenticated environment response. An empty list
   can be a valid connection; it means no environments were returned.
4. Return to Environments. Select a preview to inspect its current generation.

The browser holds the token only in memory for this session. Reloading requires
reconnecting. Disconnect clears the session. Do not paste credentials into PRs,
chat, screenshots or the URL. The API enforces authorization; a successful list
request does not establish that the token can reset or delete previews.

## Prerequisites for a first real preview

For the fastest onboarding, a platform operator should first supply a working
API URL, a registered tenant/customer cluster, a reviewed workflow commit and
a preview-only registry role. Fresh infrastructure/App registration is a
separate operator setup task and must be included when measuring that journey.

| Needed | Verification |
|---|---|
| Own Heimdall GitHub App | Installed on the selected repository; installation registered under the correct tenant |
| Customer cluster and outbound agent | Agent enrolled and installed through the chart; API cluster heartbeat is current |
| Reviewed reusable workflow | Exact `owner/repo/.github/workflows/file.yml@<40-character-sha>` registered by the operator |
| Preview registry | Repositories pre-created; CI push identity and agent pull identity are separate and scoped |
| Application | Reviewed Dockerfile, actual listening port and health behaviour; config matches the app |
| Repository Actions configuration | Required variables and exact workflow-bound OIDC trust in place |
| Access boundary | P7 gateway requirements passed before preview URLs are exposed beyond a trusted team |

An operator installs the App and enrolls the agent using
[GitHub App setup](github-app-setup.md), [control-plane setup](control-plane.md)
and [the agent chart guide](../charts/heimdall-agent/README.md). The onboarding
guide's checkmarks are progress reminders; a manually marked step is not proof
that the external installation succeeded.

## 1. Install and register the GitHub App

Follow [github-app-setup.md](github-app-setup.md). The App needs Contents read,
Pull requests write, Checks write, Actions read and Metadata read, with Pull
request and Issue comment events. It does not need repository write or workflow
write permission. Store the App private key and webhook secret outside source
control and register the actual installation/repository identifiers.

The main API webhook endpoint is `/v1/github/webhooks` (plural). The standalone
Lambda ingress template has `/v1/github/webhook` (singular). Configure the route
for the ingress you actually deployed.

## 2. Enroll and install the agent

Use the administrator's cluster creation/enrollment steps in
[control-plane.md](control-plane.md#local-startup-on-windows). Save the one-use
enrollment token to the pre-created `heimdall-agent-auth` Secret in
`heimdall-system`.

Install `charts/heimdall-agent` with `source.type=api`, the returned cluster ID,
the exact API origin, digest-pinned image and the platform's base domain,
Gateway, storage and node-pool settings. The example values are not a deployable
substitute for your cluster settings. Normal connections require HTTPS; the
documented `allowLocalHTTP=true` option is for a disposable local setup.

Verify that the agent can pull the customer registry and that the cluster
heartbeat is observed. A browser API connection alone cannot prove agent health
or gateway access control.

## 3. Prepare and review the repository files

Run from the `heimdall` directory. Replace the workflow ref with a reviewed
40-character lowercase hexadecimal commit SHA and use the application's actual
port. The following PowerShell accepts those real values before invoking the CLI:

```powershell
go build -trimpath -o out/heimdall.exe ./cmd/heimdall
$reviewedWorkflow = Read-Host 'Reviewed workflow ref: owner/repo/.github/workflows/file.yml@40-character-sha'
$applicationPort = [int](Read-Host 'Actual application listening port')
./out/heimdall.exe init --workflow $reviewedWorkflow --port $applicationPort --out-dir out/first-preview
./out/heimdall.exe validate out/first-preview/heimdall.yaml
```

`init` writes these new files and refuses to overwrite existing ones:

- `heimdall.yaml`: starter app service, real port, four-hour TTL and private visibility.
- `.github/workflows/heimdall.yml`: caller of the immutable reviewed workflow.
- `.heimdall/SETUP.md`: repository setup reminders.

It does not create a Dockerfile or commit anything. Review and adapt the starter
config to the real app, copy the files into your application's checkout and
commit them to the default branch before opening the preview PR.

Set these repository Actions variables using the platform operator's values:

| Variable | Meaning |
|---|---|
| `HEIMDALL_ROLE_ARN` | Scoped preview build/push role |
| `HEIMDALL_AWS_REGION` | Preview registry region |
| `HEIMDALL_REGISTRY` | Registry preview prefix, without a trailing slash |
| `HEIMDALL_API_URL` | Reachable HTTPS API origin, without a trailing slash |

The tenant `AllowedRegistries` entry uses the same prefix with a trailing slash.
The operator must configure the exact repository/workflow OIDC subject and
register the same trusted workflow ref and SHA. A branch name, `main`, a tag or
an all-zero example SHA is not an immutable reviewed workflow.

## 4. Open a PR and verify Ready

Open a same-repository PR with a small application change. Fork PR previews are
disabled in this implementation. Follow its GitHub Actions run, Heimdall check
and preview comment, then select the matching environment in the dashboard.

The route is **Guardrails → Dependencies → Baseline database → Application →
Smoke tests**. A pending environment may be waiting for CI images rather than
deploying. The inspector's generation and timestamp help distinguish a new push
from previously completed work.

When the observed phase is Ready, open the preview URL and exercise the app.
If deployment fails, read the diagnosis suggestion before retrying. Reset data
restores the current baseline and temporarily stops app workloads. Delete
requests cleanup; wait for the observed Destroyed state and verify namespace
removal in the live acceptance procedure.

## Common blockers

| Symptom | Next step |
|---|---|
| Browser cannot connect | Check API readiness and the `/v1` proxy; reconnect after a 401 |
| API connected, no previews | Check App installation/repository registration and signed webhook delivery |
| Pending after opening a PR | Check the trusted workflow run, repository variables, OIDC subject and accepted image/bundle digests |
| No current agent heartbeat | Check enrollment Secret, API URL, cluster ID, outbound access and agent logs |
| `IMAGE_PULL_FAILED` | Verify the reported digest exists and the agent has scoped pull access |
| `MIGRATION_FAILED` | Follow the SQLSTATE-specific suggestion; push corrected migration input |
| `POLICY_DENIED` / `trust.*` | Fit the operator policy and default-branch baseline; the PR cannot loosen them |
| 409 on a dashboard action | Refresh the environment; its version changed before the action was accepted |
| Logs unavailable/expired | Fetch a new bounded tail; log results expire after two minutes |
| Live cost shows unavailable | No validated P8 metering endpoint exists yet; sample figures are illustrative |

The full code reference is [diagnostics.md](diagnostics.md). Keep logs as a
short, explicitly requested redacted tail; durable timeline events are separate
from application log storage.

## Measure the P11 exit criterion

The P11 goal is a new user reaching a working preview in under 30 minutes using
only these docs. Record the starting prerequisites, time of the first step,
actual PR/environment IDs, first Ready timestamp and successful app visit.
Capture every point where the user needed help. A prepared developer rehearsal
or the browser-only sample cannot satisfy this acceptance criterion.
