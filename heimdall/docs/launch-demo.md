# P11 launch demonstration runbook

This runbook covers a browser rehearsal and the real ShopFlow launch
demonstration. The dashboard sample is useful for rehearsing the story. It does
not establish that GitHub, data isolation, cleanup or trust enforcement ran
successfully on a live system.

Read [dashboard-design.md](dashboard-design.md) for the product design and
[dashboard-quickstart.md](dashboard-quickstart.md) for setup. The real
demonstration is a release acceptance activity after the relevant earlier
[phase gates](phases.md) and [GitHub live acceptance](github-app-setup.md#live-acceptance-procedure).

## Launch story

Show a reviewer finding a usable PR preview, a developer correcting a failure
with a precise diagnosis and a platform operator observing cleanup and trust
enforcement. Keep the environment list visible while changing focus between
the two PRs so the isolation story is easy to follow.

Use #184 and #185 as sample PR numbers in the rehearsal. GitHub assigns actual
numbers in a live repository; record those URLs and show the real identities.
Do not create or rename resources solely to imply that the sample PRs exist.

## Browser-only rehearsal

With the frontend running, run `npm run demo:record` from `web/` to generate a
repeatable browser simulation video, desktop/mobile screenshots and
`out/p11/rehearsal.json`. The script blocks API calls. The generated
`out/p11/launch-demo-simulation.webm` is a sample presentation backup; it does
not replace the real live recording required below.

1. Start the web app and open `/dashboard?view=demo`.
2. Verify the persistent sample label is visible before advancing a scene.
3. Walk through the sample environment list and select both ShopFlow previews.
4. Show the five-stage route, Ready preview affordance and current generation.
5. Show the failed migration diagnosis and its proposed fix. Open Timeline and
   Logs to demonstrate how the evidence is accessed.
6. Exercise the sample lifecycle controls and their consequence/confirmation
   messages. Show the sample allocation estimate and explain its formula.
7. Open First preview and Docs to end on the user's path to a real setup.

Sample mode demonstrates presentation and interaction. Narrate sample state
changes as a rehearsal. A sample estimate is allocated resources × time using
illustrative rates, plus any disclosed storage/fixed allocation; it is not a
customer bill or verified cloud measurement.

## Prepare the live session

Use a disposable, authorized test repository and a local or short-lived demo
cluster. A P7-compliant private gateway is required before sharing URLs beyond
the trusted team. Use the customer-owned cluster model and the own Heimdall App.

| Preparation | Evidence to collect |
|---|---|
| P6 App/webhook/workflow registered | App/installation ID, exact trusted workflow SHA, signed delivery accepted |
| API and worker ready | Tested revision, readiness, no dead inbox/outbox blocking the demo |
| Agent enrolled and pulling API intent | Cluster ID, recent heartbeat, healthy controller/admission |
| ShopFlow reviewed and built | Config hash, actual image digests, reviewed runtime ports |
| App access boundary verified | Private gateway authentication and non-member denial evidence |
| Two disposable same-repository PRs | Actual PR URLs, separate environment IDs/namespaces |
| Approved data if needed | Content-bound sanitisation attestation and approved fixture hash |
| Recording method checked | Audio/video works, token/private-key views excluded, backup opens |
| Cleanup procedure rehearsed | Close/delete path and namespace/resource verification ready |

Heimdall defaults to schema-only databases. The data-isolation/reset scene must
use app-created test data or a reviewed manually approved sanitised fixture.
Do not describe a generated dataset as production data or imply that synthetic
seed data is a platform feature. See [engine.md](engine.md) and
[diagnostics.md](diagnostics.md).

## Scripted live demonstration

The target durations below organize the presentation; they are not product
SLOs. Actual deployment timing and cleanup time should be recorded as observed.

| Beat | Target narration | Action and proof |
|---|---|---|
| 1. Orient, 30 s | “Each PR has its own full stack and its own lifetime.” | Show both actual PRs, environment IDs and separate namespaces |
| 2. Follow deployment, 45 s | “The route tells us what is running and what is waiting.” | Select PR A; show current generation and five real stage conditions/events |
| 3. Review ready app, 45 s | “A reviewer opens this preview directly.” | Open A's real URL, perform an app operation; return to inspector |
| 4. Prove isolation, 60 s | “Changes in A stay in A.” | Create a unique test record in A; show it absent in B; keep IDs/URLs visible |
| 5. Fail a migration, 60 s | “Heimdall explains the failed stage and the next fix.” | Push a reviewed failing migration to A; wait for real `MIGRATION_FAILED`; show suggestion/evidence and generation |
| 6. Recover, 60 s | “The corrected commit is a new deployment.” | Push the fix; show new generation reaching Ready; use Retry only if appropriate for the supported failure |
| 7. Reset, 45 s | “Reset restores this preview's baseline.” | Confirm Reset data on A; observe Resetting then Ready; verify A's test changes reset and B remains unchanged |
| 8. Reject trust loosening, 45 s | “A PR cannot increase its own access.” | Attempt private → public visibility on a disposable PR/commit; show `trust.visibility` or related policy refusal and no unauthorized exposed state |
| 9. Close and cleanup, 45 s | “Closing the PR ends its environment.” | Close A; observe Destroying/Destroyed and namespace removal; B remains usable |
| 10. First preview, 30 s | “The next developer follows this guided path.” | Open setup and docs; name the actual prerequisites and link the measured onboarding evidence |

The failed-migration scenario should be deterministic and pre-rehearsed. For
example, the existing captured `migration-not-null` scenario demonstrates a
NOT NULL column without a default against approved existing rows (SQLSTATE
23502). Use its real failure mechanics as a guide; do not inject a hardcoded
diagnosis into a live environment. Keep the failure contained to the demo PR.

The trust-refusal scene must use the canonical default-branch config and a
reviewed operator policy. Do not relax tenant policy to make the demonstration
work. A UI toast or disabled action alone is not evidence of server-side refusal.

## If a live step fails

Keep the actual failure visible, name the current blocker and show the relevant
diagnosis or connection timestamp. If the presentation needs to continue,
switch explicitly to the labelled browser rehearsal or the recorded backup.
Never present a recording or sample as the current live state.

| Failure | Recovery |
|---|---|
| API/session unavailable | Verify readiness/proxy; reconnect with the current user token |
| CI stalled | Inspect workflow/OIDC/registry details; keep previous observed generation visible |
| Agent disconnected | Check heartbeat and outbound access; avoid repeating actions against unknown state |
| Migration failure differs | Use the actual code/suggestion; do not replace the displayed result |
| Action conflict | Refresh version, inspect new state, then deliberately repeat if still appropriate |
| Live route unavailable | Use diagnosis and gateway evidence; move to backup with explicit narration |
| Cleanup stuck | Keep diagnosis and evidence; use administrator break-glass only through the documented procedure |

## Recording and backup

Record a successful real run after verifying all scenes. Keep the original
recording and a playable exported copy in a durable location outside the demo
cluster. Record its date, tested revision, operating mode and actual PR IDs.
Watch the whole exported file to verify playback, readable text and audio.
Prepare a local or approved hosted copy accessible before the presentation.

Frame only the dashboard, PRs and preview app. Hide token entry, private files,
cloud credentials and unrelated account data. Show a persistent caption when
playing the backup, such as “Recorded demonstration — <date>”. The recording
is a backup, not a substitute for the independent onboarding test.

## Evidence record

Copy this table into the phase completion review and replace every pending
entry with an observed result or a link. A missing recording remains pending;
do not insert a placeholder media link.

| Item | Actual result/link |
|---|---|
| Tested Git commit and frontend build | Pending |
| Operating mode, cluster and tenant identifiers | Pending |
| App installation, trusted workflow SHA | Pending |
| Actual PR A / PR B URLs | Pending |
| Environment IDs / namespaces / generations | Pending |
| Parallel Ready previews and stage timings | Pending |
| Data isolation before and after reset | Pending |
| Failed-migration diagnosis and successful recovery | Pending |
| Trust-loosening refusal and no unauthorized exposure | Pending |
| Close-to-Destroyed and namespace removal time | Pending |
| Remaining preview still usable | Pending |
| Playable real recording and backup | Pending |
| Independent new-user first preview time and prerequisites | Pending |
| P7–P10 gate evidence | Pending |

## Finish and clean up

Close the disposable PRs and confirm their preview namespaces and owned
resources are gone. Remove disposable branches following the repository's
normal process. Preserve control-plane delivery, deployment and audit history
as evidence. If the run used billable demo infrastructure, follow the applicable
verified teardown procedure and record the orphan check result. The P9
`cloud-up`/`cloud-down` targets are planned in the roadmap; do not assume they
exist or that frontend actions tear down an AWS account.

P11 is complete only after the real demonstration, playable backup, earlier
required phase evidence and independent under-30-minute onboarding result are
recorded. Until then, describe the frontend as the implemented dashboard slice
and the live launch acceptance as pending.
