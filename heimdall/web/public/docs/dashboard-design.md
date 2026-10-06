# P11 dashboard design and implementation plan

Status: P11 implementation has started. This document defines the complete
dashboard direction, its components, its API boundaries and the evidence needed
to finish the phase. It does not supersede the release gates in
[phases.md](phases.md). Live GitHub acceptance and the P7–P10 security,
lifecycle, AWS and reliability gates must be evidenced separately.

## Implementation snapshot

The first frontend slice implements the flight deck in
`web/src/dashboard/Dashboard.tsx`, with setup/docs/rehearsal views in
`Guide.tsx`, an isolated visual theme in `dashboard.css` and an API/sample
adapter in `data.ts`. The inventory below remains the complete target design;
this snapshot distinguishes the present slice from the remaining work.

| Built in this slice | Present behaviour and limits |
|---|---|
| Flight deck and deep links | Warm canvas, dark 225 px navigation rail, editorial heading and environment runway; `/dashboard` plus setup/connections/docs/demo views |
| Responsive inspector | 360 px inspector on wide desktop; a fixed overlay on compact screens and full-width overlay on phones; Overview, Timeline and Logs tabs |
| Summary and filters | Active previews with ready count, deploying, needs attention and allocation estimate; phase/search filters; separate expiring summary/filter remains a design target |
| Environment details | Identity, deployment generation, declared workloads, five-stage route from reported conditions/steps, first diagnosis/last error, metadata and lifecycle controls |
| Preview address | One primary URL, falling back to the first reported URL; the complete additional-service URL group remains to be built |
| Live connection | Same-origin bearer API session in memory, environment/cluster reads, manual refresh plus 15-second refresh while the page is visible |
| Timeline and logs | Paginated event history on request, refresh control; 100-line redacted log requests with bounded polling; authenticated SSE remains deferred |
| Log lifetime | Server log requests/results expire after two minutes. Returned text is discarded within two minutes and on selection/generation changes; it is not saved to browser storage |
| Actions | Retry/reset/extend/delete confirmations and API requests using the observed version and idempotency header; accepted intent remains distinct from completed agent work |
| Onboarding | Four guided steps, copied starter command and locally saved self-reported checklist; workflow placeholders require replacement and verification is operator/user evidence |
| Documentation | Six guide cards, local-source reader and diagnosis pointers; safe headings, paragraphs, basic lists/tables, inline links/code/bold and fenced code. Four contract/setup references are mirrored; unpublished repository links appear as text. Nested Markdown and full cross-document navigation remain follow-up work |
| Launch rehearsal | Six explicitly illustrative scenes for parallel previews, isolation, diagnosis, reset, trust refusal and cleanup; `npm run demo:record` produces a sample WebM backup/screenshots. Real execution and recording remain acceptance work |
| Allocation display | Sample formula: 1.5 vCPU × $0.06 + 3 GiB × $0.03 = $0.18/hour; storage/shared infrastructure excluded. Live cost is unavailable until validated P8 metering exists |

The implemented base palette is approximately `#F7F6F2` canvas, `#22232B`
navigation, `#292830` primary ink and `#6555B6` violet identity, with teal
success and orange/brown attention. Exact shades can vary across component
surfaces; final contrast and browser checks apply to every essential text/control
pair. The visual-token table later in this document expresses the intended
palette and can evolve with the implementation.

Further design work includes additional service URLs, expiry-focused shortcuts,
full semantic Markdown navigation, role-aware UI backed by a real profile/session
contract, validated allocation metering and SSE. Implementation review also
covers binding a confirmation to the captured environment/version, focus
containment for compact overlays and validating nested API response data before
rendering. These refinements do not change the P11 release evidence required
for live GitHub integration, real launch recording and independent onboarding.

## Product direction: the preview flight deck

Heimdall is a place to answer three questions quickly: **Which preview can I
use? What is blocking this one? What should I do next?** The dashboard should
make those answers visible before showing secondary metrics.

The visual identity is a preview flight deck: an environment runway in the
centre, a compact navigation rail on the left and a focused inspector on the
right. Each environment is a working unit with an identity, a deployment route,
an expiry and a next action. A five-stage route makes deployment progress
legible without making users read infrastructure logs. The metaphor shapes the
layout; the actual controls keep familiar names such as Environments, Timeline,
Logs and Reset data.

The distinguishing elements are the large editorial heading, quiet warm
canvas, indigo route line, compact horizontal environment cards, a coloured
edge for the selected environment and a dark log surface inside the inspector.
Use the orange accent when attention is needed. Keep the main preview action
easy to identify on every selected environment.

## Audience and tasks

| Audience | Primary task | Successful outcome |
|---|---|---|
| Developer | Find the current PR preview, inspect a failure, retry after correcting it | Current generation and clear next action are visible in one selection |
| Reviewer or QA | Open a ready preview and check its expiry | Ready URL is easy to find; data reset consequences are clear |
| Platform operator | Connect the App/agent, diagnose connection problems, inspect policy | Setup points to the authoritative operator guides and avoids fake success |
| New team member | Understand the workflow and launch the first preview | Can follow the docs without assistance; time is measured against the P11 goal |

## Information architecture

The public landing page stays the product introduction. The dashboard lives at
`/dashboard` and uses query parameters for shareable views.

| Location | Purpose | Main components |
|---|---|---|
| `/dashboard` | Scan and operate environments | Heading, health summary, attention strip, filters, runway, inspector |
| `/dashboard?env=<id>` | Directly inspect one environment | Same runway context, selected inspector and current generation |
| `/dashboard?view=setup` | Guided first preview | Prerequisites, four-step guide, reviewed CLI command, verification checklist |
| `/dashboard?view=connections` | Connect the browser to the control plane | API session form, connection status, disconnect, operator guide links |
| `/dashboard?view=docs` | Read the documentation and find a diagnosis code | Guide index, document reader, troubleshooting cards |
| `/dashboard?view=demo` | Rehearse the launch narrative | Explicit sample marker, ordered scenes, scenario controls, evidence checklist |

Selection and view changes should support browser Back/Forward. Search, phase
filter and the selected detail tab are local presentation state. A browser
session credential never appears in the address, share link or saved UI state.

## Layout and hierarchy

Desktop uses a 208–232 px navigation column, a flexible content area and an
inspector that is about 380–440 px wide when there is enough space. The list
and inspector sit together so that switching environments preserves context.
The top row provides a short title and the API/sample indicator. Below it are
four compact operational summaries: ready, deploying, needs attention and
expiring. These are entry points into the relevant list filter.

The environment runway is the dominant region. Each row contains PR/repository
identity, branch or commit, phase, stage progress, expiry and a selection affordance.
The selected environment's inspector starts with the same identity and its
primary URL/action. It then presents deployment stages, diagnosis when present,
Timeline/Logs tabs and lifecycle controls. Destructive controls belong at the
end of the inspector and always refer to the selected environment by name.

No numerical summary should occupy more visual weight than the environment
list. A chart is useful only when a real history or meaningful comparison
exists. Empty charts, animated vanity counters and decorative radial gauges
would dilute the core task.

## Visual system

| Token | Design target | Use |
|---|---|---|
| Canvas | Warm off-white, around `#F6F5F1` | Main application background |
| Surface | White with a light neutral border | Runway rows, inspector, setup cards |
| Navigation | Deep blue-grey, around `#151A2D` | Persistent navigation rail |
| Identity | Indigo/violet, around `#5B51DB` | Selected state, primary buttons, route line |
| Success | Deep teal, around `#247566` | Ready phase and successful stage, with text/icon |
| Attention | Burnt orange, around `#B45A27` | Failure, expiry warning, attention notice |
| Text | Near-black primary and readable grey secondary | Dense data and explanations |
| Type | Existing local DM Sans/Manrope fonts | Clear UI text; monospaced logs and hashes |
| Spacing | 4 px base; 8/12/16/24/32 px steps | Consistent density across components |
| Radius | 12–18 px cards; smaller controls | Soft surfaces with clear boundaries |
| Elevation | Light border first, restrained shadow | Selected inspector and dialogs |

Verify final foreground/background pairs in the implemented theme: 4.5:1 for
normal text and 3:1 for large text and essential UI boundaries. Do not rely on
colour alone. Use a visible 2 px focus treatment with sufficient contrast.
Keep motion brief and meaningful; reduced-motion preference removes route
animation, smooth scrolling and decorative transitions. Bundle fonts and assets
locally so reading the dashboard does not depend on third-party font services.

## Complete component inventory

These are component responsibilities, not a requirement to split every small
element into its own file. Keep domain parsing separate from visual rendering.

| Component | Contents and behaviour | Required states |
|---|---|---|
| Dashboard shell | Landmark layout, skip link, navigation rail, content region | Desktop, compact, mobile, keyboard focus |
| Brand/navigation rail | Heimdall identity; Environments, First preview, Connections, Docs, Launch demo; landing link | Current item, hover, focus, mobile menu |
| Workspace header | Page title, context, refresh, sample/API mode | Connected, connecting, disconnected, stale |
| Session form | Masked token field, connect/disconnect, same-origin API explanation | Empty, validating, connected, 401, network error |
| Sample notice | Persistent explicit sample label and sample limitations | Sample active; absent for live data |
| Operational summary | Ready, deploying, attention, expiring counts; filter shortcuts | Loading, zero, populated, partial data |
| Attention strip | Most urgent issue with environment link and next step | Failure, expiry warning, connection problem, no issue |
| Runway toolbar | Labelled search, phase filters, result count, refresh | All, filtered, no matches, loading |
| Environment row | PR/repository, branch/commit, phase, generation, compact stage route, expiry | Selected, unselected, provisioning, failed, ready, destroyed |
| Phase badge | Icon and readable phase name | Every API phase; unknown phase shown safely |
| Stage route | Guardrails → Dependencies → Baseline database → Application → Smoke tests | Waiting, current, complete, failed, unknown |
| Expiry label | Relative time with absolute timestamp accessible | Future, soon, elapsed, invalid/missing |
| Inspector header | Environment identity, commit, generation, last updated, primary preview action | URL absent, ready URL, deploying, stale observation |
| Preview link group | Primary URL plus any additional service URLs | Valid HTTP(S), absent, invalid URL rejected |
| Service summary | Declared services/workers and URL associations | Metadata present, unavailable; no invented pod health |
| Diagnosis card | Stable code, summary, subject/stage, suggestion, short evidence, guide link | Current failure, secondary findings, lastError fallback, none |
| Detail tabs | Timeline and Logs with visible selected state | Keyboard navigation, narrow screen, background loading |
| Timeline | Ordered durable events with generation and timestamp, load more/refresh | Empty, pending, populated, stale generation, load error |
| Log request panel | Workload selection/input, bounded tail count, Fetch logs, returned redacted text | Idle, pending, completed, failed, expired, unsupported workload |
| Lifecycle action bar | Retry, Reset data, Extend expiry, Delete preview | Enabled, submitting, success awaiting observation, rejected/conflict |
| Reset/delete confirmation | Environment name, exact consequence, cancel, one clear action | Focus trapped, submitting, error, return focus |
| Extension form | Expiry input and explanation of policy bound | Invalid date, within UI limits, API policy rejection |
| Action feedback | Typed error or accepted intent and request correlation | 401, 403, 409, 429, server/network error, accepted |
| Allocation cost card | Estimated allocation model or honest unavailable state | Sample estimate, live unavailable, future model with source/rates |
| Empty state | Explain why there are no previews and link to setup | No data, no filter matches, disconnected |
| Setup stepper | App installation, agent connection, reviewed config/workflow, first PR | To do, self-reported, evidence verified, blocked |
| Setup command card | CLI workflow ref + real application port, copy feedback | Incomplete input, valid pinned ref, copied, clipboard unavailable |
| Setup evidence checklist | App/repo registered, agent observed, config merged, preview Ready | Self-reported progress distinguished from server verification |
| Documentation library | Task-oriented guides, diagnosis lookup, basic Markdown reader | Loading, article, missing document, load error |
| Troubleshooting card | Diagnosis code, meaning, next action, authoritative guide link | Search match, no match, code details |
| Launch scenario panel | Ordered demo beats, current scene, replay/control buttons | Sample narrative, paused/advanced, complete |
| Toast/status region | Short feedback and polite screen-reader announcements | Copy complete, action accepted, load failed |
| Error boundary/fallback | Recoverable message, retry/reload, diagnostic reference | Unexpected render/parser failure |

The landing page continues to own the product hero, architecture/security
story, feature explanation, setup excerpt, roadmap, FAQ and footer. Dashboard
links should give those elements a consistent path to the working flight deck.

## Interaction rules and honest state

1. Selection changes the inspector immediately; data fetches are scoped to that
   environment. Cancel obsolete requests when selection changes. A late response
   from PR A must never populate PR B's inspector.
2. API state is authoritative. A successful action response means intent was
   accepted; it does not mean the agent finished. Continue showing the observed
   phase until fresh status confirms the result.
3. Use the environment `version` for compare-and-swap actions. Include a fresh
   `Idempotency-Key`; retain the same key for retrying the same ambiguous request.
   On a conflict, refetch and explain that the environment changed. Do not
   silently repeat a destructive action against the new version.
4. Show deployment `generation` explicitly. Historical events retain their own
   generation, and earlier failures do not masquerade as the current failure.
   `observedGeneration` in CR status refers to Kubernetes metadata generation;
   `deployedGeneration` is the completed deployment generation. Do not confuse them.
5. The five stages come from `status.conditions` and/or the ordered `status.steps`.
   Missing status is **unknown**, not completed. A Ready phase may summarise the
   successful route, but a guessed elapsed timer never advances live deployment.
6. Preserve last good data during an outage and label its timestamp. Distinguish
   an empty successful response from a failed response. Provide a retry control.
7. Use safe HTTP(S) preview links from status. Reject unsupported protocols and
   never treat PR-controlled names, event text or log text as HTML.
8. Retry is useful after a transient issue or corrected input. Reset restores
   the current baseline and temporarily stops application workloads. Delete
   requests cleanup and keeps control-plane history. State these consequences
   in their confirmations.
9. Viewer/member/admin permissions are enforced by the API. The current API has
   no browser profile endpoint; do not infer a role from a token, a successful
   read, or the UI mode. Handle a 403 with clear permission guidance.
10. Setup checkmarks entered by the user mean **marked complete**. Reserve
    **verified** for an observed server response or an explicit evidence record.

## Current API integration boundary

The source of truth is [api/openapi.yaml](../api/openapi.yaml), with observed
status defined in [PreviewEnvironment types](../internal/api/v1alpha1/previewenvironment_types.go).

| UI need | Existing contract | Frontend responsibility |
|---|---|---|
| Environment list/detail | `GET /v1/environments`, `GET /v1/environments/{id}` | Keyset pagination (`after`, `limit`, max 100); validate JSON shape |
| Actions | `POST /v1/environments/{id}/actions` | `action`, observed `version`, optional `reason`; `expiresAt` for extend; idempotency header |
| Timeline history | `GET /v1/environments/{id}/events` | Chronological pagination; event IDs and generation retained |
| Live timeline | `GET /v1/environments/{id}/timeline` | Resumable SSE with durable ID; authenticated fetch streaming or polling |
| Short service logs | `POST /v1/environments/{id}/logs`, then `GET /v1/log-requests/{requestID}` | Workload + tail 1–200; poll only until response/deadline; logs expire after two minutes |
| Cluster connection evidence | `GET /v1/clusters` | Show name/tier/lastHeartbeat if available; avoid claims beyond the timestamp |
| Policy visibility | `GET /v1/policy` | Read-only explanatory surface initially; policy writes remain operator work |
| Enrollment | `POST /v1/clusters`, `POST /v1/clusters/{id}/enrollment` | Document operator path; enrollment is a one-use secret |

For the first implementation, use an explicit sample dataset or a same-origin
API session. Hold the user bearer token in React memory only, clear it on
disconnect and use `Authorization: Bearer …` on requests. Do not use query
tokens, local/session storage, telemetry or console logs for credentials. A
reload requires reconnecting. This is an operator/development connection flow;
a production user sign-in/session service is future backend work.

Development proxies `/v1` to the local API at `http://localhost:8080`. Production
hosting needs an HTTPS same-origin reverse proxy to the API and a fallback to
`index.html` for `/dashboard` deep links. Do not add a bearer token to native
`EventSource` URLs: browser `EventSource` cannot set an Authorization header.
The first slice can poll events; SSE can follow with an authenticated fetch
reader, cancellation, backoff and durable event-ID deduplication.

The API currently supports `retry`, `reset`, `extend` and `delete` only. It has
no sleep/wake action, billing/usage endpoint, repository-list endpoint, browser
login/profile endpoint or public launch recording. These must be explicit
follow-up work. The UI must not create convincing placeholders for missing live
facts. Live allocation cost reads **Not available** until the P8 model has a
validated backend source. Sample mode may show an illustrative estimate with
its rates and formula. It is an estimate of allocated resources, not an invoice.

## Responsive and accessibility design

| Width | Layout and priorities |
|---|---|
| Wide desktop, about 1280 px+ | Persistent rail; list and inspector together; four summary cells |
| Tablet/compact desktop, about 768–1279 px | Compact rail or top navigation; inspector below list if needed; two summary columns |
| Phone, below about 768 px | Navigation disclosure; one content column; selected inspector follows the list; full-width action controls |

These are layout triggers, not device assumptions. Verify at 390, 768, 1024 and
1440 px and at 200% zoom. Keep a minimum 44 px touch target where practical.
Log text may scroll horizontally inside its labelled container; the page must
not overflow. Long repository names, branch names, hashes and URLs wrap or
truncate with an accessible full value. Do not hide the diagnosis or primary
action on mobile.

Use semantic headings, navigation, main and complementary regions. List
selection is a button or link, not a clickable `div`. Search and forms have
visible labels; placeholders are supplementary. Tabs expose the correct tab
relationships and keyboard behaviour. Dialogs have titles, focus containment,
Escape/cancel and focus restoration. Preserve focus during refresh. Announce
action and log results politely; do not announce every timer tick or timeline
refresh. Use accessible text such as “Baseline database failed” alongside colour.

## Implementation sequence and acceptance

| Slice | Deliverable | Acceptance evidence |
|---|---|---|
| 1. Foundation | `/dashboard` routing, shell, tokens, sample marker, normalized domain adapter | TypeScript/production build; landing page still works; deep link opens |
| 2. Environment runway | Search/phase filters, rows, selection, stage route, inspector | Ready/failed/provisioning/deleted fixtures; no false stage success; mobile usable |
| 3. Existing API operations | Memory session, environment/events reads, action flow, bounded log polling | Mock contract tests for auth, conflict, request shape, stale response cancellation, logs |
| 4. First preview | Four-step setup guide, validated pinned workflow input, command copy, evidence checklist | Commands match CLI; prerequisites and operator work visible; no auto-verified self-checks |
| 5. Documentation | Local guide library, readable Markdown, diagnosis code references | Guides load; internal links work; missing/unsafe content handled |
| 6. Launch rehearsal | Labelled ShopFlow sample scenes and the real demonstration runbook | Browser scenario works; simulation and live evidence are distinguished |
| 7. Live completion | Real App/agent preview, failure/reset/cleanup/trust refusal, recording, independent onboarding | Evidence attached to release review; new user reaches Ready using only docs in <30 min |

Browser verification should exercise meaningful outcomes: filtering/selection,
direct URLs, returning between views, keyboard navigation, dialog consequences,
rejected API actions, one log request through completed/expired states, a network
error preserving prior data and sample/live separation. Reuse the existing
Playwright setup. Review desktop/mobile screenshots, reduced motion and console
errors. Add API contract coverage where adapters or mutation behaviour warrant it.

P11 completion requires the [launch runbook](launch-demo.md) and the
[first-preview guide](dashboard-quickstart.md) to work for a new user. A polished
sample dashboard or passing frontend build alone does not prove a real preview,
production access control, estimated metering accuracy or the 30-minute criterion.

## Release evidence checklist

- Record build and browser results with the commit tested.
- Record actual App installation, repository, cluster, PR/environment IDs and generations.
- Link the P6 live acceptance and P7–P10 gate evidence; identify any remaining gaps.
- Record the real demo and keep a verified playable backup outside ephemeral infrastructure.
- Observe an independent first user from prerequisites through Ready; record elapsed time and blockers.
- Verify cleanup after the demonstration and preserve audit/deployment history.
- Update the project status only after these acceptance conditions are met.
