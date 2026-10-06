### Preview failed: Resource quota exceeded

**`QUOTA_EXCEEDED`** in `deployment/web` (stage: `application`)

web cannot create pods: the preview's resource quota is exhausted (requested limits.cpu=500m; used limits.cpu=6100m of limits.cpu=6500m)

**What to do:** The preview's quota is sized from heimdall.yaml, so this happens when something adds pods beyond it (a manual scale, extra replicas, a stuck rollout keeping old pods). Remove the extra pods, or raise `resources` in heimdall.yaml within the tenant's limits.

<details><summary>Evidence</summary>

```text
FailedCreate: Error creating: pods "web-c556bf97c-qrl2h" is forbidden: exceeded quota: heimdall-quota, requested: limits.cpu=500m, used: limits.cpu=6100m, limited: limits.cpu=6500m
quota heimdall-quota: limits.cpu used 6100m of 6500m
```

</details>

<sub>`heimdall-pr407-shopflow-4130` · generation 1 · run `heimdall diagnose` for details</sub>
