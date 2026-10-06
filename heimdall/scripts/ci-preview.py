"""Trusted workflow helper: PR input is data; builds run inside BuildKit.

Never invoke repository scripts or interpolate PR text into shell code.
Registry credentials have only customer preview-prefix permissions.
"""
import json
import os
import pathlib
import re
import subprocess
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def execute(args, cwd, capture=False):
    return subprocess.run(args, cwd=cwd, check=True, text=True,
                          stdout=subprocess.PIPE if capture else None).stdout


def contained(root, value):
    path = (root / value).resolve(strict=True)
    if not path.is_relative_to(root):
        raise ValueError("build paths must stay inside the PR checkout")
    return str(path)


def main():
    cli = os.environ["HEIMDALL_CLI"]
    root = pathlib.Path(os.environ["HEIMDALL_APPLICATION"]).resolve(strict=True)
    registry = os.environ["HEIMDALL_REGISTRY"]
    if not re.fullmatch(r"[0-9]{12}\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com(?:\.cn)?/[a-z0-9][a-z0-9_./-]*", registry):
        raise ValueError("registry must be a customer ECR preview prefix")
    platforms = os.environ["HEIMDALL_PLATFORMS"].split(",")
    if not platforms or len(set(platforms)) != len(platforms) or any(p not in ("linux/amd64", "linux/arm64") for p in platforms):
        raise ValueError("only explicit amd64/arm64 build platforms are accepted")
    endpoint = urllib.parse.urlsplit(os.environ["HEIMDALL_API_URL"])
    if endpoint.scheme != "https" or not endpoint.hostname or endpoint.username or endpoint.query or endpoint.fragment or endpoint.path not in ("", "/"):
        raise ValueError("control API must be an HTTPS origin")
    api = urllib.parse.urlunsplit(endpoint).rstrip("/")
    sha = os.environ["HEIMDALL_HEAD_SHA"]
    if not re.fullmatch(r"[a-f0-9]{40,64}", sha):
        raise ValueError("invalid PR head SHA")
    tag = "{}-{}-{}".format(sha, int(os.environ["GITHUB_RUN_ID"]), int(os.environ["GITHUB_RUN_ATTEMPT"]))
    # build-plan validates bounded syntax. Deployment authorization uses the
    # current tenant policy and canonical baseline at the control plane/agent.
    plan = json.loads(execute([cli, "build-plan"], root, True))
    images = {}
    with tempfile.TemporaryDirectory(prefix="heimdall-ci-") as temporary:
        temp = pathlib.Path(temporary)
        for item in plan:
            name = item["name"]
            if not re.fullmatch(r"[a-z][a-z0-9-]{0,62}", name):
                raise ValueError("invalid workload name")
            if "build" in item:
                build = item["build"]
                context = pathlib.Path(contained(root, build["Context"]))
                dockerfile = contained(root, str(context.relative_to(root) / build["Dockerfile"]))
                reference = "{}/{}:{}".format(registry, name, tag)
                metadata = temp / (name + ".json")
                command = ["docker", "buildx", "build", "--push", "--platform", ",".join(platforms),
                           "--provenance=mode=max", "--sbom=true", "--metadata-file", str(metadata),
                           "--tag", reference, "--file", dockerfile]
                for key, value in sorted((build.get("Args") or {}).items()):
                    command += ["--build-arg", "{}={}".format(key, value)]
                execute(command + [str(context)], root)
                digest = json.loads(metadata.read_text())["containerimage.digest"]
                if not re.fullmatch(r"sha256:[a-f0-9]{64}", digest):
                    raise ValueError("builder returned an invalid digest")
                image = "{}/{}@{}".format(registry, name, digest)
            else:
                image = item["image"]
                if not re.fullmatch(r"[A-Za-z0-9._:/-]+@sha256:[a-f0-9]{64}", image):
                    raise ValueError("prebuilt images must be pinned by SHA-256")
            for platform in platforms:
                execute(["trivy", "image", "--no-progress", "--scanners", "vuln", "--severity", "HIGH,CRITICAL",
                         "--exit-code", "1", "--platform", platform, image], root)
            images[name] = image
        layout = str(temp / "bundle")
        execute([cli, "bundle", "pack", "--repo-root", str(root), "--layout", layout], root)
        bundle = execute([cli, "bundle", "push", "--layout", layout, "--reference", registry + "/bundle:" + tag], root, True).strip()
    payload = json.dumps({"repositoryID": int(os.environ["HEIMDALL_REPOSITORY_ID"]), "pullRequest": int(os.environ["HEIMDALL_PR"]),
                          "commit": sha, "images": images, "bundle": bundle}).encode()
    oidc_url = os.environ["ACTIONS_ID_TOKEN_REQUEST_URL"] + "&audience=" + urllib.parse.quote(api, safe="")
    request = urllib.request.Request(oidc_url, headers={"Authorization": "Bearer " + os.environ["ACTIONS_ID_TOKEN_REQUEST_TOKEN"]})
    with urllib.request.urlopen(request, timeout=15) as response:
        token = json.load(response)["value"]
    request = urllib.request.Request(api + "/v1/github/builds", data=payload,
                                     headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"}, method="POST")
    # Retry transient reception races without rebuilding or changing payload.
    for attempt in range(6):
        try:
            # HTTPS origins are checked; forbid credential-bearing redirects.
            opener = urllib.request.build_opener(NoRedirect)
            with opener.open(request, timeout=45) as response:
                if response.status not in (200, 201, 202):
                    raise RuntimeError("unexpected build callback result")
            print("Heimdall accepted immutable preview build digests")
            return
        except urllib.error.HTTPError as error:
            if error.code not in (404, 425, 429, 502, 503, 504) or attempt == 5:
                raise RuntimeError("Heimdall build callback rejected ({})".format(error.code)) from None
            time.sleep(min(2 ** attempt, 16))


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


if __name__ == "__main__":
    main()
