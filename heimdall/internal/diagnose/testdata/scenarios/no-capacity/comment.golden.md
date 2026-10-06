### Preview failed: No capacity to schedule

**`NO_CAPACITY`** in `deployment/rabbitmq` (stage: `dependencies`)

rabbitmq cannot be scheduled: no node matches the preview node pool

**What to do:** The platform pins previews to a node pool (platform.nodeSelector/tolerations) that has no matching, untainted node. A platform administrator should check the pool.

<details><summary>Evidence</summary>

```text
0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.
FailedScheduling: 0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.
```

</details>

<details><summary>Also found (2)</summary>

- **`NO_CAPACITY`** redis cannot be scheduled: no node matches the preview node pool  
  The platform pins previews to a node pool (platform.nodeSelector/tolerations) that has no matching, untainted node. A platform administrator should check the pool.
- **`NO_CAPACITY`** postgres cannot be scheduled: no node matches the preview node pool  
  The platform pins previews to a node pool (platform.nodeSelector/tolerations) that has no matching, untainted node. A platform administrator should check the pool.

</details>

<sub>`heimdall-pr409-shopflow-3e82` · generation 1 · run `heimdall diagnose` for details</sub>
