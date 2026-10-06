import { cp, mkdir, copyFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const repository = path.resolve(web, "..");
const destination = path.join(web, "public", "docs");
await mkdir(destination, { recursive: true });
await cp(path.join(repository, "docs"), destination, { recursive: true });
for (const reference of [
  "api/openapi.yaml",
  "internal/api/v1alpha1/previewenvironment_types.go",
  "web/README.md",
  "charts/heimdall-agent/README.md",
]) {
  const target = path.join(destination, "reference", reference);
  await mkdir(path.dirname(target), { recursive: true });
  await copyFile(path.join(repository, reference), target);
}
console.log("Local documentation and public contract references refreshed.");
