### Preview failed: Image cannot be pulled

**`IMAGE_PULL_FAILED`** in `job/heimdall-migrate-g1` (stage: `baseline-db`)

The migration Job cannot pull image localhost:5004/shopflow-api@sha256:abababababababababababababababababababababababababababababababab: it does not exist in the registry

**What to do:** The digest is not in the registry. Make sure CI pushed the image before requesting the preview, and that the digest and repository name match what it pushed.

<details><summary>Evidence</summary>

```text
ImagePullBackOff: Back-off pulling image "localhost:5004/shopflow-api@sha256:abababababababababababababababababababababababababababababababab": ErrImagePull: rpc error: code = NotFound desc = failed to pull and unpack image "localhost:5004…
Failed: Failed to pull image "localhost:5004/shopflow-api@sha256:abababababababababababababababababababababababababababababababab": rpc error: code = NotFound desc = failed to pull and unpack image "localhost:5004/shopflow-api@sha256:ababa…
Failed: Error: ErrImagePull (x4)
Failed: Error: ImagePullBackOff (x7)
```

</details>

<sub>`heimdall-pr401-shopflow-e5b8` · generation 1 · run `heimdall diagnose` for details</sub>
