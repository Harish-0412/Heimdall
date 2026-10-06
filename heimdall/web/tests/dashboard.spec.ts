import { expect, test, type Page, type Route } from "@playwright/test";
import type { Environment } from "../src/dashboard/data";

const TOKEN = "dashboard-test-token-memory-only";

function liveEnvironment(overrides: Partial<Environment> = {}): Environment {
  const now = Date.now();
  return {
    id: "env-live-71",
    tenantID: "tenant-acme",
    repositoryID: "repository-catalog",
    clusterID: "cluster-acme",
    name: "catalog-pr-71",
    version: 7,
    generation: 2,
    resetNonce: 0,
    desiredState: "Running",
    phase: "Ready",
    expiresAt: new Date(now + 4 * 60 * 60 * 1000).toISOString(),
    createdAt: new Date(now - 10 * 60 * 1000).toISOString(),
    updatedAt: new Date(now - 60 * 1000).toISOString(),
    spec: {
      repository: "acme/catalog",
      pullRequest: 71,
      commit: "a".repeat(40),
      generation: 2,
      images: { web: `registry.example.com/web@sha256:${"b".repeat(64)}` },
      config: {
        inline:
          "version: 1\nservices:\n  web:\n    port: 3000\npreview:\n  visibility: private\n",
        sha256: "c".repeat(64),
      },
    },
    status: {
      deployedGeneration: 2,
      namespace: "catalog-pr71-x4k9",
      steps: [
        { name: "guardrails/setup", state: "succeeded", durationSeconds: 8 },
      ],
      urls: [
        {
          service: "web",
          url: "https://catalog.preview.example.com",
          primary: true,
        },
      ],
    },
    ...overrides,
  };
}

interface APIStub {
  environments: Environment[];
  requests: {
    method: string;
    path: string;
    body: unknown;
    authorization?: string;
    idempotencyKey?: string;
  }[];
  error?: { status: number; code: string; message: string; requestID: string };
  onAction?: (route: Route) => Promise<void>;
  onEnvironments?: (route: Route) => Promise<void>;
  onLogs?: (route: Route) => Promise<void>;
  onReadLogs?: (route: Route) => Promise<void>;
}

async function mockAPI(
  page: Page,
  environments = [liveEnvironment()],
): Promise<APIStub> {
  const api: APIStub = { environments, requests: [] };
  await page.route("**/v1/**", async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const headers = request.headers();
    api.requests.push({
      method: request.method(),
      path: url.pathname,
      body: request.postData() ? request.postDataJSON() : null,
      authorization: headers.authorization,
      idempotencyKey: headers["idempotency-key"],
    });
    if (api.error) {
      await route.fulfill({ status: api.error.status, json: api.error });
    } else if (url.pathname === "/v1/environments" && api.onEnvironments) {
      await api.onEnvironments(route);
    } else if (url.pathname === "/v1/environments") {
      await route.fulfill({ json: { items: api.environments } });
    } else if (url.pathname === "/v1/clusters") {
      await route.fulfill({
        json: [
          {
            id: "cluster-acme",
            tenantID: "tenant-acme",
            name: "Customer cluster",
            tier: 1,
            lastHeartbeat: new Date().toISOString(),
          },
        ],
      });
    } else if (url.pathname.endsWith("/actions") && api.onAction) {
      await api.onAction(route);
    } else if (url.pathname.endsWith("/logs") && api.onLogs) {
      await api.onLogs(route);
    } else if (url.pathname.startsWith("/v1/log-requests/") && api.onReadLogs) {
      await api.onReadLogs(route);
    } else if (url.pathname.endsWith("/events")) {
      const environmentID = url.pathname.split("/")[3];
      await route.fulfill({
        json: {
          items: [
            {
              id: 22,
              environmentID,
              generation: 2,
              type: "stage",
              message: "stage",
              data: {
                step: "baseline-db/migrate",
                state: "failed",
                code: "MIGRATION_FAILED",
                durationSeconds: 11,
              },
              createdAt: new Date().toISOString(),
            },
          ],
          next: 22,
        },
      });
    } else {
      const env = api.environments.find(
        (environment) => environment.id === url.pathname.split("/")[3],
      );
      await route.fulfill({
        status: env ? 200 : 404,
        json: env ?? {
          code: "api.not_found",
          message: "Resource not found",
          requestID: "request-test",
        },
      });
    }
  });
  return api;
}

async function noOverflow(page: Page) {
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
}

async function navigate(page: Page, name: string) {
  const nav = page.getByRole("navigation", { name: "Dashboard navigation" });
  const open = page.getByRole("button", {
    name: "Open navigation",
    exact: true,
  });
  if (await open.isVisible()) await open.click();
  await nav
    .getByRole("button", { name, exact: name !== "Environments" })
    .click();
}

async function connect(page: Page) {
  await page
    .getByRole("button", { name: "Connect your control plane", exact: true })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Connect your control plane",
  });
  await dialog.getByLabel("Tenant token").fill(TOKEN);
  await dialog
    .getByRole("button", { name: "Connect workspace", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByText("Live API", { exact: true })).toBeVisible();
}

const inspector = (page: Page) =>
  page.getByRole("complementary", { name: "Environment details" });
const board = (page: Page) =>
  page.getByRole("region", { name: "Environments", exact: true });

test("sample overview is honest, responsive and free of browser errors", async ({
  page,
}, testInfo) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.goto("/dashboard");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Your next change, in view.",
  );
  await expect(
    page.getByText("Sample workspace", { exact: true }),
  ).toBeVisible();
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(4);
  await expect(
    page.getByText("Sample estimate · not a bill", { exact: true }),
  ).toBeVisible();
  if (testInfo.project.name === "mobile") {
    for (let index = 0; index < 8; index++) {
      await page.keyboard.press("Tab");
      expect(
        await page.evaluate(() =>
          Boolean(document.activeElement?.closest("#fd-navigation")),
        ),
      ).toBe(false);
    }
  }
  await noOverflow(page);
  await board(page).scrollIntoViewIfNeeded();
  await noOverflow(page);
  await page.screenshot({
    path: testInfo.outputPath("dashboard-overview.png"),
    fullPage: true,
    animations: "disabled",
  });
  expect(errors).toEqual([]);
});

test("filters and search compose and can recover from no results", async ({
  page,
}) => {
  await page.goto("/dashboard");
  const filters = page.getByRole("group", { name: "Filter environments" });
  await filters.getByRole("button", { name: "Ready", exact: true }).click();
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(2);
  await expect(
    filters.getByRole("button", { name: "Ready", exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  await page
    .getByRole("textbox", { name: "Search environments" })
    .fill("shopflow");
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(1);
  await expect(
    board(page).getByRole("button", {
      name: "Inspect shopflow #184",
      exact: true,
    }),
  ).toBeVisible();
  await page
    .getByRole("textbox", { name: "Search environments" })
    .fill("missing-project");
  await expect(
    page.getByRole("heading", { name: "No previews match your view." }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Clear filters", exact: true })
    .click();
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(4);
  await expect(
    page.getByRole("textbox", { name: "Search environments" }),
  ).toHaveValue("");
  await filters.getByRole("button", { name: "Deploying", exact: true }).click();
  await expect(
    board(page).getByRole("button", {
      name: "Inspect storefront #42",
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(1);
});

test("environment inspector exposes the real pipeline, services, timeline and sample logs", async ({
  page,
}, testInfo) => {
  await page.goto("/dashboard");
  await page
    .getByRole("button", { name: "Inspect shopflow #184", exact: true })
    .click();
  const details = inspector(page);
  await expect(details).toBeVisible();
  await expect(
    details.getByRole("heading", { name: "Deployment path" }),
  ).toBeVisible();
  await expect(
    details.getByText("baseline-db/migrate", { exact: true }),
  ).toBeVisible();
  await expect(details.getByText("3 declared", { exact: true })).toBeVisible();
  await expect(
    details.getByRole("link", { name: "Open preview", exact: true }),
  ).toHaveCount(0);
  await details
    .getByRole("button", { name: "View api logs", exact: true })
    .click();
  await expect(
    details.getByRole("tab", { name: "Logs", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(
    details.getByRole("combobox", { name: "Log workload" }),
  ).toHaveValue("api");
  await expect(
    details.getByText(/\[sample logs · api · secrets redacted\]/),
  ).toBeVisible();
  await details.getByRole("tab", { name: "Timeline", exact: true }).click();
  await expect(
    details.getByText("Sample pull request opened. Preview intent accepted.", {
      exact: true,
    }),
  ).toBeVisible();
  await details.getByRole("tab", { name: "Timeline", exact: true }).focus();
  await page.keyboard.press("ArrowRight");
  await expect(
    details.getByRole("tab", { name: "Logs", exact: true }),
  ).toBeFocused();
  await expect(
    details.getByRole("tab", { name: "Logs", exact: true }),
  ).toHaveAttribute("aria-selected", "true");
  await noOverflow(page);
  await page.screenshot({
    path: testInfo.outputPath("dashboard-inspector.png"),
    animations: "disabled",
  });
  await details
    .getByRole("button", { name: "Close environment details" })
    .click();
  await expect(details).toHaveCount(0);
  await expect(page).not.toHaveURL(/env=/);
});

test("failed diagnosis provides evidence and reset requires a focused confirmation", async ({
  page,
}) => {
  await page.goto("/dashboard");
  await page
    .getByRole("button", { name: /One preview needs a little attention/ })
    .click();
  const details = inspector(page);
  await expect(
    details.getByText("MIGRATION_FAILED", { exact: true }),
  ).toBeVisible();
  await expect(
    details.getByText("The discount migration references a missing column.", {
      exact: true,
    }),
  ).toBeVisible();
  await details.getByText("View evidence", { exact: true }).click();
  await expect(
    details.getByText(/column "discount_type" does not exist/),
  ).toBeVisible();
  const reset = details.getByRole("button", { name: "Reset", exact: true });
  await reset.click();
  const dialog = page.getByRole("dialog", { name: "Reset the preview data?" });
  await expect(dialog).toBeVisible();
  await expect(
    dialog.getByText(/Preview data changes will be lost/),
  ).toBeVisible();
  for (let index = 0; index < 6; index++) {
    await page.keyboard.press("Tab");
    expect(
      await page.evaluate(() =>
        Boolean(document.activeElement?.closest("dialog")),
      ),
    ).toBe(true);
  }
  await page.keyboard.press("Escape");
  await expect(dialog).toHaveCount(0);
  await expect(reset).toBeFocused();
  await expect(
    details.getByText("Generation 1", { exact: true }),
  ).toBeVisible();
  await reset.click();
  await dialog
    .getByRole("button", { name: "Simulate reset", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect(details.getByText("Resetting", { exact: true })).toBeVisible();
  await expect(
    details.getByText("Generation 2", { exact: true }),
  ).toBeVisible();
  await expect(
    details.getByText("MIGRATION_FAILED", { exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByText("Sample reset applied. No infrastructure was changed.", {
      exact: true,
    }),
  ).toBeVisible();
});

test("sample extension preserves generation and delete remains a desired-state transition", async ({
  page,
}) => {
  await page.goto("/dashboard?env=shopflow-184");
  const details = inspector(page);
  await details.getByRole("button", { name: "Extend", exact: true }).click();
  const extend = page.getByRole("dialog", {
    name: "Give this preview more time.",
  });
  await extend
    .getByLabel("New expiry (UTC)")
    .fill(new Date(Date.now() + 72 * 3600000).toISOString());
  await extend
    .getByRole("button", { name: "Simulate extend", exact: true })
    .click();
  await expect(
    details.getByText("Generation 1", { exact: true }),
  ).toBeVisible();
  await details
    .getByRole("button", { name: "Delete environment", exact: true })
    .click();
  const remove = page.getByRole("dialog", { name: "Delete this environment?" });
  await expect(
    remove.getByText(/Deployment and audit history are retained/),
  ).toBeVisible();
  await remove.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(details.getByText("Ready", { exact: true })).toBeVisible();
  await details
    .getByRole("button", { name: "Delete environment", exact: true })
    .click();
  await remove
    .getByRole("button", { name: "Simulate delete", exact: true })
    .click();
  await expect(details.getByText("Destroying", { exact: true })).toBeVisible();
  await expect(
    details.getByText("Generation 2", { exact: true }),
  ).toBeVisible();
  await expect(
    details.getByRole("button", { name: "Retry", exact: true }),
  ).toBeDisabled();
  await expect(
    details.getByRole("button", { name: "Delete environment", exact: true }),
  ).toBeDisabled();
});

test("live connection follows pagination, shows heartbeat and keeps credentials in memory", async ({
  page,
}) => {
  const first = liveEnvironment();
  const second = liveEnvironment({
    id: "env-live-72",
    name: "catalog-pr-72",
    spec: {},
    status: {},
    phase: "Pending",
  });
  const api = await mockAPI(page, [first, second]);
  api.onEnvironments = async (route) => {
    const after = new URL(route.request().url()).searchParams.get("after");
    await route.fulfill({
      json: after ? { items: [second] } : { items: [first], next: first.id },
    });
  };
  await page.goto("/dashboard");
  await connect(page);
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(2);
  await expect(
    page.getByText("Cost data not available", { exact: true }),
  ).toBeVisible();
  expect(
    api.requests.every(
      (request) => request.authorization === `Bearer ${TOKEN}`,
    ),
  ).toBe(true);
  expect(
    await page.evaluate(() =>
      [...Object.values(localStorage), ...Object.values(sessionStorage)].join(
        " ",
      ),
    ),
  ).not.toContain(TOKEN);
  await page
    .getByRole("button", { name: "Inspect acme/catalog #71", exact: true })
    .click();
  await inspector(page)
    .getByRole("tab", { name: "Timeline", exact: true })
    .click();
  const history = inspector(page).locator(".fd-timeline");
  await expect(history).toContainText("baseline-db/migrate");
  await expect(history).toContainText("failed");
  await expect(history).toContainText("MIGRATION_FAILED");
  await expect(
    history.locator("li p").filter({ hasText: "baseline-db/migrate" }),
  ).toContainText(/11\s*(s\b|seconds\b)/);
  await inspector(page)
    .getByRole("button", { name: "Close environment details", exact: true })
    .click();
  await navigate(page, "Connections");
  await expect(
    page.getByText("Customer cluster", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Last seen Just now", { exact: true }),
  ).toBeVisible();
  await navigate(page, "Environments");
  const help = page.getByRole("button", { name: "Help", exact: true });
  if (await help.isVisible()) await help.click();
  else await navigate(page, "Documentation");
  await expect(
    page.getByRole("heading", { name: "The field guide.", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Live API", { exact: true })).toBeVisible();
  await page.keyboard.press("Control+k");
  await expect(
    page.getByRole("textbox", { name: "Search environments", exact: true }),
  ).toBeFocused();
  await expect(page.getByText("Live API", { exact: true })).toBeVisible();
  await expect(
    board(page).getByRole("button", { name: /^Inspect / }),
  ).toHaveCount(2);
  await page
    .getByRole("button", {
      name: "Inspect repository-catalog catalog-pr-72",
      exact: true,
    })
    .click();
  await expect(
    inspector(page).getByText("Commit unavailable", { exact: true }),
  ).toBeVisible();
  await page.reload();
  await expect(
    page.getByText("Sample workspace", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Connect your control plane", exact: true })
    .click();
  await expect(page.getByLabel("Tenant token")).toHaveValue("");
});

test("live CAS conflict preserves observations and requires rereading before a new action", async ({
  page,
}) => {
  const api = await mockAPI(page);
  api.onAction = async (route) => {
    api.environments = [liveEnvironment({ version: 8 })];
    await route.fulfill({
      status: 409,
      json: {
        code: "state.version_conflict",
        message: "The environment changed; read the current version and retry",
        requestID: "request-cas-409",
      },
    });
  };
  await page.goto("/dashboard");
  await connect(page);
  await page
    .getByRole("button", { name: "Inspect acme/catalog #71", exact: true })
    .click();
  await inspector(page)
    .getByRole("button", { name: "Reset", exact: true })
    .click();
  const dialog = page.getByRole("dialog", { name: "Reset the preview data?" });
  await dialog
    .getByRole("button", { name: "Confirm reset", exact: true })
    .click();
  await expect(dialog.getByRole("alert")).toContainText(
    "The environment changed",
  );
  await expect(
    dialog.getByRole("button", { name: "Confirm reset", exact: true }),
  ).toBeDisabled();
  const original = api.requests.find(
    (request) => request.method === "POST" && request.path.endsWith("/actions"),
  );
  expect(original?.body).toEqual({ action: "reset", version: 7 });
  expect(original?.idempotencyKey).toMatch(/^[0-9a-f-]{36}$/);
  await expect(
    inspector(page).getByText("Generation 2", { exact: true }),
  ).toBeVisible();
  await dialog
    .getByRole("button", { name: "Refresh environment and review" })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect
    .poll(
      () =>
        api.requests.filter((request) => request.path === "/v1/environments")
          .length,
    )
    .toBeGreaterThan(1);
  await inspector(page)
    .getByRole("button", { name: "Reset", exact: true })
    .click();
  await expect(dialog.getByText(/generation 2 · version 8/)).toBeVisible();
  api.onAction = async (route) => {
    const next = liveEnvironment({
      version: 9,
      generation: 3,
      resetNonce: 1,
      phase: "Resetting",
      status: {},
    });
    api.environments = [next];
    await route.fulfill({ json: next });
  };
  await dialog
    .getByRole("button", { name: "Confirm reset", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect(
    inspector(page).getByText("Generation 3", { exact: true }),
  ).toBeVisible();
  await expect(
    inspector(page).getByText("Resetting", { exact: true }),
  ).toBeVisible();
  expect(
    api.requests.filter((request) => request.path.endsWith("/actions")).at(-1)
      ?.body,
  ).toEqual({ action: "reset", version: 8 });
});

test("live logs poll the agent and report viewer permission failures without stale log text", async ({
  page,
}) => {
  const api = await mockAPI(page);
  const logRequest = {
    id: "log-request-1",
    environmentID: "env-live-71",
    generation: 2,
    workload: "web",
    tail: 100,
    state: "pending",
    expiresAt: new Date(Date.now() + 120000).toISOString(),
  };
  api.onLogs = (route) => route.fulfill({ status: 202, json: logRequest });
  api.onReadLogs = (route) =>
    route.fulfill({
      json: {
        ...logRequest,
        state: "completed",
        text: "[web/app]\nINFO ready\nDATABASE_PASSWORD=[REDACTED]",
      },
    });
  await page.goto("/dashboard");
  await connect(page);
  await page
    .getByRole("button", { name: "Inspect acme/catalog #71", exact: true })
    .click();
  const details = inspector(page);
  await details
    .getByRole("button", { name: "View web logs", exact: true })
    .click();
  await expect(
    details.getByText("Waiting for the agent…", { exact: true }),
  ).toBeVisible();
  await expect(
    details.getByText(/DATABASE_PASSWORD=\[REDACTED\]/),
  ).toBeVisible();
  expect(
    api.requests.find((request) => request.path.endsWith("/logs"))?.body,
  ).toEqual({ workload: "web", tail: 100 });
  expect(
    api.requests.some(
      (request) => request.path === "/v1/log-requests/log-request-1",
    ),
  ).toBe(true);
  api.onLogs = (route) =>
    route.fulfill({
      status: 403,
      json: {
        code: "auth.forbidden",
        message: "This credential is not authorized for this operation",
        requestID: "request-viewer-403",
      },
    });
  await details
    .getByRole("button", { name: "Refresh logs", exact: true })
    .click();
  await expect(details.getByRole("alert")).toContainText("not authorized");
  await expect(details.getByText(/DATABASE_PASSWORD=\[REDACTED\]/)).toHaveCount(
    0,
  );
});

test("invalid authentication leaves sample data and clears the entered credential on cancel", async ({
  page,
}) => {
  const api = await mockAPI(page);
  api.error = {
    status: 401,
    code: "auth.invalid",
    message: "The credential is invalid, expired or revoked",
    requestID: "request-auth-401",
  };
  await page.goto("/dashboard");
  await page
    .getByRole("button", { name: "Connect your control plane", exact: true })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Connect your control plane",
  });
  await dialog.getByLabel("Tenant token").fill(TOKEN);
  await dialog
    .getByRole("button", { name: "Connect workspace", exact: true })
    .click();
  await expect(dialog.getByRole("alert")).toContainText(
    "invalid, expired or revoked",
  );
  await expect(
    page.getByText("Sample workspace", { exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(() =>
      [...Object.values(localStorage), ...Object.values(sessionStorage)].join(
        " ",
      ),
    ),
  ).not.toContain(TOKEN);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await page
    .getByRole("button", { name: "Connect your control plane", exact: true })
    .click();
  await expect(dialog.getByLabel("Tenant token")).toHaveValue("");
  await noOverflow(page);
});

test("empty workspaces guide setup and refresh failures retain the last successful snapshot", async ({
  page,
}) => {
  const api = await mockAPI(page, []);
  await page.goto("/dashboard");
  await connect(page);
  await expect(
    page.getByRole("heading", { name: "Your first preview starts here." }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Start setup", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: /One PR away/ }),
  ).toBeVisible();
  await navigate(page, "Environments");
  api.environments = [liveEnvironment()];
  await page
    .getByRole("button", { name: "Refresh environments", exact: true })
    .click();
  await expect(
    board(page).getByRole("button", {
      name: "Inspect acme/catalog #71",
      exact: true,
    }),
  ).toBeVisible();
  api.error = {
    status: 503,
    code: "api.unavailable",
    message: "The control plane is temporarily unavailable",
    requestID: "request-refresh-503",
  };
  await page
    .getByRole("button", { name: "Refresh environments", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText(
    "Showing the last successful snapshot.",
  );
  await expect(
    board(page).getByRole("button", {
      name: "Inspect acme/catalog #71",
      exact: true,
    }),
  ).toBeVisible();
  api.error = undefined;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("onboarding is a persistent manual checklist and documentation handles search and load failures", async ({
  page,
  context,
}) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.goto("/dashboard?view=setup");
  await expect(
    page.getByText(
      "Steps are self-reported; connection status is verified separately.",
      { exact: false },
    ),
  ).toBeVisible();
  const steps = page.getByRole("region", { name: "Onboarding steps" });
  await steps
    .getByRole("checkbox", { name: "I’ve completed and verified this step" })
    .check();
  await expect(
    page.getByText("1 of 4 checked off", { exact: true }),
  ).toBeVisible();
  await steps.getByRole("button", { name: /Define your preview/ }).click();
  await steps
    .getByRole("button", { name: "Copy command", exact: true })
    .click();
  expect(await page.evaluate(() => navigator.clipboard.readText())).toContain(
    "--workflow 'OWNER/REPO/",
  );
  await page.reload();
  await expect(
    page.getByText("1 of 4 checked off", { exact: true }),
  ).toBeVisible();
  await expect(
    steps.getByRole("checkbox", {
      name: "I’ve completed and verified this step",
    }),
  ).toBeChecked();
  await navigate(page, "Documentation");
  await page.getByRole("button", { name: /Find the root cause/ }).click();
  const reader = page.getByRole("region", { name: "Documentation reader" });
  await expect(
    reader.getByRole("heading", {
      name: "Diagnostics: what failed and what to do",
      exact: true,
    }),
  ).toBeVisible();
  await page.route("**/docs/dashboard-quickstart.md", (route) =>
    route.fulfill({ status: 503, body: "Unavailable" }),
  );
  await page.getByRole("button", { name: /Your first preview/ }).click();
  await expect(reader.getByRole("alert")).toContainText(
    "This guide could not be loaded",
  );
  await page
    .getByRole("textbox", { name: "Search documentation" })
    .fill("not-a-guide");
  await expect(
    page.getByRole("heading", { name: "No guides match that search." }),
  ).toBeVisible();
  await page
    .getByRole("textbox", { name: "Search documentation" })
    .fill("cluster");
  await expect(
    page.getByRole("button", { name: /Connect your cluster/ }),
  ).toBeVisible();
  await noOverflow(page);
});

test("all six launch scenes are reachable and explicitly illustrative", async ({
  page,
}, testInfo) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.goto("/dashboard");
  await navigate(page, "Launch demo");
  await expect(
    page.getByText("Browser simulation", { exact: true }),
  ).toBeVisible();
  const titles = [
    "Two PRs. Two worlds.",
    "Change one. Keep the other.",
    "A failure with a way forward.",
    "Back to a clean baseline.",
    "Keep the guardrails in place.",
    "Review finished. Resources gone.",
  ];
  const actions = [
    "Next: data isolation",
    "Next: diagnose a failure",
    "Next: reset the data",
    "Next: refuse unsafe input",
    "Next: close and clean up",
    "Replay the story",
  ];
  for (let index = 0; index < titles.length; index++) {
    await expect(
      page.getByRole("heading", { name: titles[index], exact: true }),
    ).toBeVisible();
    await expect(
      page.getByText("ILLUSTRATIVE EVIDENCE", { exact: true }),
    ).toBeVisible();
    await noOverflow(page);
    await page
      .getByRole("button", { name: actions[index], exact: true })
      .click();
  }
  await expect(
    page.getByRole("heading", { name: titles[0], exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("dashboard-launch-demo.png"),
    fullPage: true,
    animations: "disabled",
  });
  if (testInfo.project.name === "mobile") {
    await page
      .getByRole("button", { name: "Open navigation", exact: true })
      .click();
    await page
      .getByRole("navigation", { name: "Dashboard navigation" })
      .getByRole("button", { name: "Getting started", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "Open navigation", exact: true }),
    ).toHaveAttribute("aria-expanded", "false");
  }
});
