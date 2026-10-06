# P5 control-plane acceptance

Run from the Heimdall module with Docker Desktop (or Docker Engine), kind,
kubectl, Helm, Go, and Bash available:

```bash
bash test/e2e/control/run.sh
```

The script builds the current agent and ShopFlow images, pushes them to a
dedicated local customer registry, and installs two agent replicas in a kind
cluster. The test starts real PostgreSQL using the production migrations and
restricted application role. The central API accepts user intent, while the
agent registers, pulls desired state, and reports status through its outbound
channel. Kubernetes reads verify the resulting resources; enrollment bootstrap
and Helm installation prepare the agent before the API acceptance begins.

The acceptance covers API create, retry, reset, expiry extension without losing
application data, durable stage history after an API restart, bounded on-demand
logs, two isolated previews, agent restart with the persisted session, an API
outage beyond the sweep grace period, first-build OCI failure diagnosis, and
delete after policy tightening. A reviewed synthetic product fixture travels in
the digest-pinned registry artifact with explicit sanitised approval, reason,
and an exact data hash in the administrator's test intent. The agent fetches
and binds its immutable fixture ConfigMap to the preview's exact UID; deletion
removes only that preview's fixture. Fixture SQL is absent from central API
metadata. Local HTTP and anonymous registry access are explicit test settings.

The default cluster is `heimdall-p5`, registry is `heimdall-p5-registry`, and host
registry port is `5005`. Use a fresh cluster without a previous agent install.
`CLUSTER`, `REGISTRY`, and `REGISTRY_PORT` can select another dedicated test
cluster. `KEEP_CLUSTER=1` preserves it for inspection; otherwise the script
removes its kind cluster and registry after the test. PostgreSQL is removed by
test cleanup. The test fails when required infrastructure is unavailable.

The separate real PostgreSQL/Redis API integration suite verifies shared log
delivery between API replicas and ephemeral Redis retention. This kind test
uses an in-process API with a deliberate outage switch to exercise the complete
outbound agent path. GitHub's real-repository exit criterion belongs to P6.
