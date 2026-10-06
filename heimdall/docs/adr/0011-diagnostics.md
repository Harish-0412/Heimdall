# ADR 0011: Diagnostics as rules over redacted snapshots

Status: accepted for P4

## Context

"Your preview failed" is useless; "your migration adds a NOT NULL column
without a default to a table with rows; add a DEFAULT" is the product. The
explanation must be correct, stable enough for automation (PR comments,
dashboards, alerts), safe to show to everyone who can read a pull request,
and fast: a crash loop should not take the step timeout (10 minutes) to report.

## Decisions

**A pure function over a snapshot.** `diagnose.Diagnose(Snapshot) Report`
has no I/O. `diagnose.Collect` captures the snapshot (pods, events, Jobs,
workloads, services and endpoint slices, quotas, HTTPRoutes with the Gateway's
verdict, log tails, the engine's recorded failure and accepted generation)
separately. The same snapshot always yields the same report, so rules are
tested against snapshots captured from real failures on kind (one scenario
per code), and a user can attach a snapshot to a bug report. A failure
refused before anything existed is diagnosed from the failure alone.

**Stable public codes.** Fourteen codes from the plan, plus `UNCLASSIFIED`
so that a failure is never reported without a diagnosis (it carries the
engine's own code and message, and flags a missing rule). Codes are never
renamed or reused.

**Ranking by causality, not by severity.** Stage order first (a migration
failure outranks an API that is not ready; a crashing PostgreSQL outranks the
app that cannot reach it), then a tier per code, then dependencies first.
Symptoms are suppressed when their cause is explained for the same workload.
Only the current generation is considered, which makes the ranking immune to
history in the namespace (tested by injecting noise and requiring an
identical report).

**Specific over generic.** Rules parse what the platform and the app say:
PostgreSQL SQLSTATEs as printed by the common clients, curl's error codes,
probe failures (status code, refused, timeout), scheduler and quota messages,
Gateway route conditions, exit codes, and the app's first error line.
Suggestions name the field in `heimdall.yaml` (or the platform setting) to
change, and whose fix it is when it is the platform's.

**The URL is part of the preview.** Workloads can all be Ready while the
Gateway refuses the route (a base domain outside its listener, a listener that
does not admit preview namespaces). A rejected route is a `NO_ENDPOINTS` error
of its own, never suppressed as a symptom. A route without status says
nothing: no controller has judged it.

**Redaction: exact values first, fail closed.** The collector redacts the
values Heimdall injected, verbatim and encoded, then applies patterns as a
safety net. If those values cannot be read, it collects no logs. Environment
values, managed fields and last-applied annotations never enter a snapshot.
PostgreSQL row values in error details are redacted too. Everything is capped.
The redactor is shared with `heimdall logs` (`internal/redact`), so there is
one implementation. It now keeps the host of a connection URL (diagnosis)
while removing its password.

**Renderers treat app output as hostile.** Text from the preview (log lines,
identifiers) goes into code spans and fences sized to what they contain;
other prose is escaped, so app output cannot inject markdown or HTML into a
pull-request comment. The comment's exact text is pinned by tests.

**Fail fast, from the same knowledge.** While waiting for a workload, the
engine checks its pods every 10 seconds with `diagnose.Blocked` and stops on a
failure that cannot recover without new input (crash loop, repeated OOM,
persistent pull failure, configuration error), as the terminal
`engine.workload_failed`. Scheduling problems wait: an autoscaler may help.

**Where diagnoses go.** The CLI prints them when `up`/`reset` fail and on
`heimdall diagnose`. The agent diagnoses inside the failed operation's
goroutine (bounded at 30s, never in Reconcile) and records the top five in
`status.diagnoses`, the root cause in the `Ready` message and a `Diagnosed`
event; success clears them. A spec rejected before any operation is diagnosed
from its configuration diagnostics alone.

## Consequences

- New failure modes need a rule and, ideally, a captured scenario; until then
  they surface as `UNCLASSIFIED` with the engine's code.
- Fail-fast makes some failures terminal sooner: an image pushed more than two
  minutes after the preview was requested needs a new generation. That matches
  the CI contract (P6): push the images, then request the preview.
- The agent's preview role gains read access to EndpointSlices.
- LLM summarisation, if added, sits on top of these rules, after redaction.

## Completion review

Known-value redaction includes short values and URL-safe base64, and runs before
multiline splitting or truncation. Secret discovery includes the pods' actual
references. When values cannot be read, all free-form evidence is omitted;
confirmed missing Secret identities are retained separately from their values.
Public structural identifiers remain available for generation fencing and
workload matching. Failed diagnosis collection still produces a fallback report.

Current-state filtering also checks workload templates, object UIDs, Gateway
observed generation and recovery timestamps. Collection limits prioritize
current warnings, and the error retained before a large dump survives through
the final diagnosis. Every captured scenario has JSON, text and markdown goldens.

Lost operation-journal ownership remains the primary failure even when workload
symptoms are present: `UNCLASSIFIED` carries `engine.lock_lost`, while independent
findings stay secondary. This avoids recommending an application change for a
cluster/API interruption.
