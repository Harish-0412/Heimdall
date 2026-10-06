### Preview failed: Service has no ready endpoints

**`NO_ENDPOINTS`** in `httproute/web` (stage: `application`)

http://pr408-shopflow-1236.wrong.test is not served: Gateway heimdall-gateway/heimdall (listener http) rejected route web (NoMatchingListenerHostname)

**What to do:** The preview's hostname (http://pr408-shopflow-1236.wrong.test) is outside every hostname the Gateway listens on. The platform's baseDomain must be the zone of the listener's wildcard hostname (for example \*.preview.example.com): a platform administrator's fix, in the agent's values or the CLI's --base-domain.

<details><summary>Evidence</summary>

```text
heimdall-gateway/heimdall (listener http) Accepted=False NoMatchingListenerHostname: The Listener hostname does not match the Route hostnames
```

</details>

<sub>`heimdall-pr408-shopflow-1236` · generation 1 · run `heimdall diagnose` for details</sub>
