"""Build a static Linux arm64 Lambda bootstrap with correct ZIP permissions."""
import argparse
import os
from pathlib import Path
import subprocess
import tempfile
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default="out/webhook.zip")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    output = Path(args.output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    if output.exists():
        raise SystemExit("Output already exists; choose a new artifact path")
    with tempfile.TemporaryDirectory(prefix="heimdall-webhook-") as temporary:
        bootstrap = Path(temporary) / "bootstrap"
        environment = dict(os.environ, GOOS="linux", GOARCH="arm64", CGO_ENABLED="0")
        subprocess.run(["go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(bootstrap), "./cmd/webhook"],
                       cwd=root, env=environment, check=True)
        info = zipfile.ZipInfo("bootstrap", date_time=(1980, 1, 1, 0, 0, 0))
        info.create_system = 3
        info.external_attr = 0o100755 << 16
        info.compress_type = zipfile.ZIP_DEFLATED
        with zipfile.ZipFile(output, "x") as archive:
            archive.writestr(info, bootstrap.read_bytes())
    print("Built Linux arm64 webhook artifact:", output)


if __name__ == "__main__":
    main()
