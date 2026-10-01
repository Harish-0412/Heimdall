import { test, expect } from "@playwright/test";

test("page and images load without errors or horizontal overflow", async ({
  page,
}, testInfo) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/");
  await expect(page.getByRole("heading", { level: 1 })).toContainText(
    "Every pull request.",
  );
  await expect(
    page.getByRole("heading", { name: "The foundation is here." }),
  ).toBeAttached();
  await expect(page.locator(".brand-mark img").first()).toBeVisible();
  for (const selector of [
    ".hero-copy",
    ".hero-cards",
    ".preview-card",
    ".ecosystem-card",
  ]) {
    expect(
      await page.locator(selector).evaluate((el) => {
        const bounds = el.getBoundingClientRect();
        return bounds.left >= 0 && bounds.right <= window.innerWidth;
      }),
    ).toBe(true);
  }
  expect(
    await page
      .locator(".brand-mark img")
      .first()
      .evaluate(
        (img: HTMLImageElement) => img.complete && img.naturalWidth > 0,
      ),
  ).toBe(true);
  expect((await page.request.get("/images/hero-landscape.webp")).ok()).toBe(
    true,
  );
  for (const section of [
    "#top",
    "#features",
    "#get-started",
    ".final-cta",
    ".footer",
  ]) {
    await page.locator(section).scrollIntoViewIfNeeded();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
  }
  await page.evaluate(() => window.scrollTo({ top: 0, behavior: "instant" }));
  await page.screenshot({
    path: testInfo.outputPath("hero.png"),
    animations: "disabled",
  });
  for (const element of await page.locator(".reveal").all())
    await element.scrollIntoViewIfNeeded();
  await page.screenshot({
    path: testInfo.outputPath("full-page.png"),
    fullPage: true,
    animations: "disabled",
  });
  expect(errors).toEqual([]);
});

test("walkthrough supports tabs, replay and escape dismissal", async ({
  page,
}) => {
  await page.goto("/");
  const trigger = page.getByRole("button", {
    name: "See how it works",
    exact: true,
  });
  await trigger.click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  await dialog.getByRole("tab", { name: "Services", exact: true }).click();
  await expect(
    dialog.getByText("Notifications", { exact: true }),
  ).toBeVisible();
  await dialog.getByRole("tab", { name: "Logs", exact: true }).click();
  await expect(dialog.locator(".demo-logs")).toContainText(
    "[ready] all smoke checks passed",
  );
  await dialog.getByRole("button", { name: "Replay deployment" }).click();
  await expect(dialog.getByRole("tab", { name: "Pipeline" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
  await expect(dialog.getByText("Deploying", { exact: true })).toBeVisible();
  await expect(dialog.getByText("Preview ready", { exact: true })).toBeVisible({
    timeout: 8000,
  });
  await expect(
    dialog.getByRole("button", { name: "Replay deployment" }),
  ).toBeEnabled();
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(trigger).toBeFocused();
});

test("setup tabs and clipboard provide working code", async ({
  page,
  context,
  browserName,
}) => {
  if (browserName === "chromium")
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/");
  await page.getByRole("button", { name: "Show quick-start commands" }).click();
  await expect(
    page.getByRole("tab", { name: "Quick start", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(page.locator(".code-window pre")).toContainText(
    "go build -o bin/heimdall",
  );
  await page.getByRole("button", { name: "Copy code" }).click();
  await expect(page.getByRole("status")).toHaveText("Copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toContain(
    "heimdall render",
  );
  await page.getByRole("tab", { name: "heimdall.yaml" }).click();
  await expect(page.locator(".code-window pre")).toContainText(
    "visibility: private",
  );
  await page.getByRole("button", { name: "Copy code" }).click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toContain(
    "dependencies:",
  );
});

test("documentation assets and FAQ disclosures work", async ({ page }) => {
  await page.goto("/");
  await page
    .getByRole("button", { name: "Read the docs", exact: true })
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  for (const link of await dialog.locator(".doc-links a").all()) {
    const response = await page.request.get((await link.getAttribute("href"))!);
    expect(response.ok()).toBe(true);
    expect(await response.text()).toMatch(/^# /);
  }
  await dialog.getByRole("button", { name: "Close dialog" }).click();
  await expect(dialog).toHaveCount(0);
  await page.getByText("Do I need an AWS account?", { exact: true }).click();
  await expect(page.locator("details[open]")).toContainText(
    "No. Start with the CLI without cluster access.",
  );
});

test("mobile navigation and reduced motion remain usable", async ({
  page,
}, testInfo) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/");
  if (testInfo.project.name === "mobile") {
    await page.getByRole("button", { name: "Open menu" }).click();
    await expect(page.getByRole("navigation")).toBeVisible();
    await page
      .getByRole("navigation")
      .getByRole("link", { name: "Security", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "Open menu" }),
    ).toHaveAttribute("aria-expanded", "false");
    await expect(page).toHaveURL(/#security$/);
  }
  expect(
    await page
      .locator(".marquee-track")
      .evaluate((el) => getComputedStyle(el).animationName),
  ).toBe("none");
  expect(
    await page
      .locator(".reveal")
      .first()
      .evaluate((el) => getComputedStyle(el).opacity),
  ).toBe("1");
});
