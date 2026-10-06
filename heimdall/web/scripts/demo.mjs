import { chromium } from "@playwright/test";
import { mkdir, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import path from "node:path";

const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const output = path.resolve(web, "../out/p11");
const origin = process.env.HEIMDALL_DEMO_ORIGIN || "http://127.0.0.1:5173";
await mkdir(output, { recursive: true });
const browser = await chromium.launch();
try {
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1000 },
    reducedMotion: "reduce",
    recordVideo: { dir: output, size: { width: 1440, height: 1000 } },
  });
  const page = await context.newPage();
  let apiCalls = 0;
  await page.route("**/v1/**", (route) => {
    apiCalls++;
    return route.abort();
  });
  await page.goto(`${origin}/dashboard`, { waitUntil: "networkidle" });
  await page.screenshot({
    path: path.join(output, "dashboard-desktop.png"),
    animations: "disabled",
  });
  await page
    .getByRole("button", { name: "Inspect shopflow #185", exact: true })
    .click();
  await page.screenshot({
    path: path.join(output, "dashboard-diagnosis.png"),
    animations: "disabled",
  });
  await page.goto(`${origin}/dashboard?view=demo`, {
    waitUntil: "networkidle",
  });
  for (const action of [
    "Next: data isolation",
    "Next: diagnose a failure",
    "Next: reset the data",
    "Next: refuse unsafe input",
    "Next: close and clean up",
  ]) {
    await page.waitForTimeout(2500);
    await page.getByRole("button", { name: action, exact: true }).click();
  }
  await page.waitForTimeout(2500);
  await page.screenshot({
    path: path.join(output, "launch-demonstration.png"),
    animations: "disabled",
  });
  await context.close();
  await page.video().saveAs(path.join(output, "launch-demo-simulation.webm"));
  const mobile = await browser.newContext({
    viewport: { width: 390, height: 844 },
    reducedMotion: "reduce",
    isMobile: true,
    hasTouch: true,
  });
  const mobilePage = await mobile.newPage();
  await mobilePage.goto(`${origin}/dashboard`, { waitUntil: "networkidle" });
  await mobilePage.screenshot({
    path: path.join(output, "dashboard-mobile.png"),
    fullPage: true,
    animations: "disabled",
  });
  await mobile.close();
  if (apiCalls)
    throw new Error(
      "The simulation attempted an API call; do not use this recording as a sample backup.",
    );
  await writeFile(
    path.join(output, "rehearsal.json"),
    JSON.stringify(
      {
        kind: "browser simulation",
        recordedAt: new Date().toISOString(),
        apiCalls,
        liveAcceptance: "not performed",
        recording: "launch-demo-simulation.webm",
        views: [
          "overview",
          "diagnosis",
          "six launch scenes",
          "mobile overview",
        ],
      },
      null,
      2,
    ),
  );
  console.log(
    `Sample rehearsal and screenshots saved to ${output}. Live launch acceptance remains separate.`,
  );
} finally {
  await browser.close();
}
