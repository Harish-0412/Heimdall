/** The public /v1 contract. Tokens and transient log responses stay in memory. */
export type ActionName = "retry" | "reset" | "extend" | "delete";

export interface RawSpec {
  traceParent?: string;
  tenant?: string;
  repository?: string;
  pullRequest?: number;
  commit?: string;
  generation?: number;
  environmentID?: string;
  owner?: string;
  urlSuffix?: string;
  expiresAt?: string;
  desiredState?: "Running" | "Destroyed";
  resetNonce?: number;
  config?: {
    inline: string;
    sha256: string;
    bundle?: string;
    baseline?: string;
    baselineSHA256?: string;
    approvedBy?: string;
  };
  images?: Record<string, string>;
  data?: {
    configMap: string;
    key: string;
    approval: {
      sha256: string;
      approvedBy: string;
      reason: string;
      sanitised: boolean;
    };
  };
  /** Sample-only display metadata; the current live API does not expose branches. */
  branch?: string;
}

export interface EnvironmentStatus {
  observedGeneration?: number;
  deployedGeneration?: number;
  completedResetNonce?: number;
  phase?: string;
  namespace?: string;
  urls?: { service: string; url: string; primary?: boolean }[];
  steps?: {
    name: string;
    state: string;
    startedAt?: string;
    durationSeconds?: number;
    code?: string;
  }[];
  diagnoses?: {
    code: string;
    summary: string;
    suggestion: string;
    evidence?: string[];
    subject?: string;
    stage?: string;
  }[];
  lastError?: {
    code: string;
    message: string;
    step?: string;
    generation: number;
    retryable: boolean;
    at: string;
  };
  conditions?: {
    type: string;
    status: string;
    observedGeneration?: number;
    lastTransitionTime: string;
    reason: string;
    message: string;
  }[];
  operation?: {
    type: string;
    generation: number;
    resetNonce?: number;
    result: string;
    attempts?: number;
    startedAt: string;
    completedAt?: string;
    retryAfter?: string;
  };
}

export interface Environment {
  id: string;
  tenantID: string;
  repositoryID: string;
  clusterID: string;
  name: string;
  version: number;
  generation: number;
  resetNonce: number;
  desiredState: "Running" | "Destroyed";
  phase: string;
  spec: RawSpec;
  status: EnvironmentStatus;
  expiresAt: string;
  createdAt: string;
  updatedAt: string;
}

export interface Event {
  id: number;
  environmentID: string;
  generation: number;
  type: string;
  message: string;
  data?: Record<string, unknown>;
  createdAt: string;
}

export interface Cluster {
  id: string;
  tenantID: string;
  name: string;
  tier: number;
  lastHeartbeat?: string;
}

interface LogRequest {
  id: string;
  environmentID: string;
  generation: number;
  workload: string;
  tail: number;
  state: "pending" | "completed" | "failed";
  expiresAt: string;
  text?: string;
  error?: string;
}

export class APIError extends Error {
  constructor(
    message: string,
    public readonly status: number,
    public readonly code: string,
    public readonly requestID: string = "",
  ) {
    super(message);
    this.name = "APIError";
  }
}

const PAGE_SIZE = 100;
const MAX_PAGES = 50;
const object = (value: unknown): value is Record<string, unknown> =>
  typeof value === "object" && value !== null && !Array.isArray(value);
const invalidResponse = () =>
  new APIError(
    "The server returned an unexpected response. Refresh and try again.",
    502,
    "api.invalid_response",
  );
type Validator = (value: unknown) => boolean;
const string: Validator = (value) => typeof value === "string";
const boolean: Validator = (value) => typeof value === "boolean";
const integer: Validator = (value) =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
const date: Validator = (value) =>
  typeof value === "string" && Number.isFinite(Date.parse(value));
const optional = (
  record: Record<string, unknown>,
  key: string,
  validate: Validator,
): boolean => record[key] === undefined || validate(record[key]);
const requiredStrings = (
  record: Record<string, unknown>,
  keys: string[],
): boolean => keys.every((key) => string(record[key]));
const optionalStrings = (
  record: Record<string, unknown>,
  keys: string[],
): boolean => keys.every((key) => optional(record, key, string));
const arrayOf =
  (validate: Validator): Validator =>
  (value) =>
    Array.isArray(value) && value.every(validate);
const desiredState: Validator = (value) =>
  value === "Running" || value === "Destroyed";

function isRawSpec(value: unknown): value is RawSpec {
  if (
    !object(value) ||
    !optionalStrings(value, [
      "traceParent",
      "tenant",
      "repository",
      "commit",
      "environmentID",
      "owner",
      "urlSuffix",
      "branch",
    ]) ||
    !["pullRequest", "generation", "resetNonce"].every((key) =>
      optional(value, key, integer),
    ) ||
    !optional(value, "expiresAt", date) ||
    !optional(value, "desiredState", desiredState)
  )
    return false;
  if (
    !optional(
      value,
      "images",
      (images) => object(images) && Object.values(images).every(string),
    )
  )
    return false;
  if (
    !optional(
      value,
      "config",
      (config) =>
        object(config) &&
        requiredStrings(config, ["inline", "sha256"]) &&
        optionalStrings(config, [
          "bundle",
          "baseline",
          "baselineSHA256",
          "approvedBy",
        ]),
    )
  )
    return false;
  return optional(
    value,
    "data",
    (data) =>
      object(data) &&
      requiredStrings(data, ["configMap", "key"]) &&
      object(data.approval) &&
      requiredStrings(data.approval, ["sha256", "approvedBy", "reason"]) &&
      boolean(data.approval.sanitised),
  );
}

function isEnvironmentStatus(value: unknown): value is EnvironmentStatus {
  if (
    !object(value) ||
    !optionalStrings(value, ["phase", "namespace"]) ||
    !["observedGeneration", "deployedGeneration", "completedResetNonce"].every(
      (key) => optional(value, key, integer),
    )
  )
    return false;
  if (
    !optional(
      value,
      "urls",
      arrayOf(
        (url) =>
          object(url) &&
          requiredStrings(url, ["service", "url"]) &&
          optional(url, "primary", boolean),
      ),
    )
  )
    return false;
  if (
    !optional(
      value,
      "steps",
      arrayOf(
        (step) =>
          object(step) &&
          requiredStrings(step, ["name", "state"]) &&
          optional(step, "startedAt", date) &&
          optional(step, "durationSeconds", integer) &&
          optional(step, "code", string),
      ),
    )
  )
    return false;
  if (
    !optional(
      value,
      "diagnoses",
      arrayOf(
        (diagnosis) =>
          object(diagnosis) &&
          requiredStrings(diagnosis, ["code", "summary", "suggestion"]) &&
          optionalStrings(diagnosis, ["subject", "stage"]) &&
          optional(diagnosis, "evidence", arrayOf(string)),
      ),
    )
  )
    return false;
  if (
    !optional(
      value,
      "lastError",
      (error) =>
        object(error) &&
        requiredStrings(error, ["code", "message"]) &&
        optional(error, "step", string) &&
        integer(error.generation) &&
        boolean(error.retryable) &&
        date(error.at),
    )
  )
    return false;
  if (
    !optional(
      value,
      "conditions",
      arrayOf(
        (condition) =>
          object(condition) &&
          requiredStrings(condition, ["type", "reason", "message"]) &&
          typeof condition.status === "string" &&
          ["True", "False", "Unknown"].includes(condition.status) &&
          optional(condition, "observedGeneration", integer) &&
          date(condition.lastTransitionTime),
      ),
    )
  )
    return false;
  return optional(
    value,
    "operation",
    (operation) =>
      object(operation) &&
      requiredStrings(operation, ["type", "result"]) &&
      integer(operation.generation) &&
      ["resetNonce", "attempts"].every((key) =>
        optional(operation, key, integer),
      ) &&
      date(operation.startedAt) &&
      ["completedAt", "retryAfter"].every((key) =>
        optional(operation, key, date),
      ),
  );
}

function isEnvironment(value: unknown): value is Environment {
  if (!object(value)) return false;
  return (
    requiredStrings(value, [
      "id",
      "tenantID",
      "repositoryID",
      "clusterID",
      "name",
      "phase",
    ]) &&
    ["expiresAt", "createdAt", "updatedAt"].every((key) => date(value[key])) &&
    ["version", "generation", "resetNonce"].every((key) =>
      integer(value[key]),
    ) &&
    desiredState(value.desiredState) &&
    isRawSpec(value.spec) &&
    isEnvironmentStatus(value.status)
  );
}

function isEvent(value: unknown): value is Event {
  return (
    object(value) &&
    integer(value.id) &&
    integer(value.generation) &&
    requiredStrings(value, ["environmentID", "type", "message"]) &&
    date(value.createdAt) &&
    optional(value, "data", object)
  );
}

function isLogRequest(value: unknown): value is LogRequest {
  return (
    object(value) &&
    requiredStrings(value, ["id", "environmentID", "workload"]) &&
    integer(value.generation) &&
    typeof value.tail === "number" &&
    Number.isInteger(value.tail) &&
    value.tail >= 1 &&
    value.tail <= 200 &&
    (value.state === "pending" ||
      value.state === "completed" ||
      value.state === "failed") &&
    date(value.expiresAt) &&
    optional(
      value,
      "text",
      (text) =>
        typeof text === "string" &&
        new TextEncoder().encode(text).length <= 16_384,
    ) &&
    optional(
      value,
      "error",
      (error) =>
        typeof error === "string" &&
        new TextEncoder().encode(error).length <= 256,
    )
  );
}

function delay(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(
        signal.reason ?? new DOMException("Request cancelled", "AbortError"),
      );
      return;
    }
    const abort = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", abort);
      reject(
        signal.reason ?? new DOMException("Request cancelled", "AbortError"),
      );
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", abort);
      resolve();
    }, ms);
    signal.addEventListener("abort", abort, { once: true });
  });
}

export class DashboardAPI {
  private readonly token: string;

  constructor(token: string) {
    this.token = token.trim();
  }

  private async request(
    path: string,
    options: RequestInit = {},
  ): Promise<unknown> {
    if (!this.token)
      throw new APIError(
        "Enter a dashboard access token to connect.",
        401,
        "auth.required",
      );
    const controller = new AbortController();
    const abort = () => controller.abort(options.signal?.reason);
    if (options.signal?.aborted) abort();
    else options.signal?.addEventListener("abort", abort, { once: true });
    const timer = setTimeout(
      () =>
        controller.abort(
          new APIError("The request timed out. Try again.", 408, "api.timeout"),
        ),
      15_000,
    );
    try {
      const headers = new Headers(options.headers);
      headers.set("Authorization", `Bearer ${this.token}`);
      headers.set("Accept", "application/json");
      if (options.body) headers.set("Content-Type", "application/json");
      const response = await fetch(`/v1${path}`, {
        ...options,
        headers,
        signal: controller.signal,
        credentials: "same-origin",
        cache: "no-store",
      });
      let body: unknown;
      try {
        body = await response.json();
      } catch {
        throw new APIError(
          "The server returned an unreadable response.",
          response.status,
          "api.invalid_response",
          response.headers.get("X-Request-ID") ?? "",
        );
      }
      if (!response.ok) {
        const detail = object(body) ? body : {};
        throw new APIError(
          typeof detail.message === "string"
            ? detail.message
            : "The request could not be completed.",
          response.status,
          typeof detail.code === "string" ? detail.code : "api.request_failed",
          typeof detail.requestID === "string"
            ? detail.requestID
            : (response.headers.get("X-Request-ID") ?? ""),
        );
      }
      return body;
    } catch (error) {
      if (controller.signal.aborted)
        throw (
          controller.signal.reason ??
          new DOMException("Request cancelled", "AbortError")
        );
      if (error instanceof APIError) throw error;
      throw new APIError(
        "Could not reach the control plane. Check your connection and try again.",
        0,
        "api.network",
      );
    } finally {
      clearTimeout(timer);
      options.signal?.removeEventListener("abort", abort);
    }
  }

  async environments(): Promise<Environment[]> {
    const all: Environment[] = [];
    const seen = new Set<string>();
    let after = "";
    for (let page = 0; page < MAX_PAGES; page++) {
      const response = await this.request(
        `/environments?limit=${PAGE_SIZE}${after ? `&after=${encodeURIComponent(after)}` : ""}`,
      );
      if (
        !object(response) ||
        !Array.isArray(response.items) ||
        response.items.length > PAGE_SIZE ||
        !response.items.every(isEnvironment)
      )
        throw invalidResponse();
      all.push(...response.items);
      if (response.next === undefined || response.next === "") return all;
      if (
        typeof response.next !== "string" ||
        seen.has(response.next) ||
        response.items.length === 0
      )
        throw invalidResponse();
      after = response.next;
      seen.add(after);
    }
    throw new APIError(
      "The environment list exceeded its page limit. No incomplete list has been shown.",
      413,
      "api.page_limit",
    );
  }

  async events(id: string): Promise<Event[]> {
    const all: Event[] = [];
    let after = 0;
    for (let page = 0; page < MAX_PAGES; page++) {
      const response = await this.request(
        `/environments/${encodeURIComponent(id)}/events?limit=${PAGE_SIZE}&after=${after}`,
      );
      if (
        !object(response) ||
        !Array.isArray(response.items) ||
        response.items.length > PAGE_SIZE ||
        !response.items.every(isEvent) ||
        !Number.isSafeInteger(response.next)
      )
        throw invalidResponse();
      const items = response.items;
      const next = response.next as number;
      if (
        items.some(
          (event, index) =>
            event.environmentID !== id ||
            event.id <= (index === 0 ? after : items[index - 1].id),
        ) ||
        next !== (items.at(-1)?.id ?? after)
      )
        throw invalidResponse();
      all.push(...items);
      // This endpoint returns the last event ID even on the final page.
      if (items.length < PAGE_SIZE) return all;
      after = next;
    }
    throw new APIError(
      "The timeline exceeded its page limit. No incomplete timeline has been shown.",
      413,
      "api.page_limit",
    );
  }

  async clusters(): Promise<Cluster[]> {
    const response = await this.request("/clusters");
    if (
      !Array.isArray(response) ||
      !response.every(
        (value) =>
          object(value) &&
          requiredStrings(value, ["id", "tenantID", "name"]) &&
          integer(value.tier) &&
          optional(value, "lastHeartbeat", date),
      )
    )
      throw invalidResponse();
    return response as Cluster[];
  }

  async act(
    env: Environment,
    action: ActionName,
    expiresAt?: string,
  ): Promise<Environment> {
    if (
      action === "extend" &&
      (!expiresAt ||
        !Number.isFinite(Date.parse(expiresAt)) ||
        Date.parse(expiresAt) <= Date.parse(env.expiresAt))
    ) {
      throw new APIError(
        "Choose an expiry after the current expiry.",
        400,
        "api.invalid_expiry",
      );
    }
    const response = await this.request(
      `/environments/${encodeURIComponent(env.id)}/actions`,
      {
        method: "POST",
        headers: { "Idempotency-Key": crypto.randomUUID() },
        body: JSON.stringify({
          action,
          version: env.version,
          ...(action === "extend" ? { expiresAt } : {}),
        }),
      },
    );
    if (!isEnvironment(response) || response.id !== env.id)
      throw invalidResponse();
    return response;
  }

  async logs(
    env: Environment,
    workload: string,
    signal?: AbortSignal,
  ): Promise<string> {
    if (
      !/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(workload) ||
      workload.length > 63
    )
      throw new APIError(
        "Choose a declared application workload.",
        400,
        "api.invalid_workload",
      );
    const controller = new AbortController();
    const abort = () => controller.abort(signal?.reason);
    if (signal?.aborted) abort();
    else signal?.addEventListener("abort", abort, { once: true });
    const timer = setTimeout(
      () =>
        controller.abort(
          new APIError(
            "The agent did not return logs within 30 seconds. Request a new tail.",
            408,
            "logs.timeout",
          ),
        ),
      30_000,
    );
    try {
      let response = await this.request(
        `/environments/${encodeURIComponent(env.id)}/logs`,
        {
          method: "POST",
          body: JSON.stringify({ workload, tail: 100 }),
          signal: controller.signal,
        },
      );
      let requestID = "";
      for (;;) {
        if (!isLogRequest(response)) throw invalidResponse();
        const request = response;
        if (
          request.environmentID !== env.id ||
          request.generation !== env.generation ||
          request.workload !== workload ||
          (requestID && request.id !== requestID)
        ) {
          throw new APIError(
            "The environment changed while logs were being fetched. Refresh and request a new tail.",
            409,
            "logs.stale_generation",
          );
        }
        if (Date.parse(request.expiresAt) <= Date.now())
          throw new APIError(
            "This log request expired. Request a new tail.",
            410,
            "logs.expired",
          );
        if (request.state === "failed")
          throw new APIError(
            request.error || "The agent could not retrieve this log tail.",
            502,
            "logs.failed",
          );
        if (request.state === "completed") return request.text ?? "";
        requestID = request.id;
        await delay(1_000, controller.signal);
        response = await this.request(
          `/log-requests/${encodeURIComponent(requestID)}`,
          { signal: controller.signal },
        );
      }
    } finally {
      clearTimeout(timer);
      signal?.removeEventListener("abort", abort);
    }
  }
}

export function environmentMeta(env: Environment): {
  repository: string;
  branch: string;
  pr: number | null;
  commit: string;
  services: string[];
  visibility: string;
} {
  const spec = env.spec;
  // This is a display hint, not an authoritative YAML parser or policy check.
  const visibility =
    spec.config?.inline.match(
      /^\s+visibility:\s*(private|org|public)\s*(?:#.*)?$/m,
    )?.[1] ?? "Unknown";
  return {
    repository: spec.repository || env.repositoryID,
    branch: spec.branch || "Branch unavailable",
    pr: spec.pullRequest && spec.pullRequest > 0 ? spec.pullRequest : null,
    commit: spec.commit || "Commit unavailable",
    services: Object.keys(spec.images ?? {}).length
      ? Object.keys(spec.images ?? {})
      : [...new Set((env.status.urls ?? []).map((url) => url.service))],
    visibility,
  };
}

function sampleConfig(services: string[]): string {
  const image = (service: string) =>
    `ghcr.io/heimdall-demo/${service}@sha256:${"a".repeat(64)}`;
  const application = services.filter((service) => service !== "worker");
  const declarations = application.map(
    (service) =>
      `  ${service}:\n    image: ${image(service)}\n    port: ${service === "api" ? 8080 : 3000}${service === application[0] ? "\n    public: true\n    primary: true" : service === "api" ? "\n    public: true" : ""}`,
  );
  const worker = services.includes("worker")
    ? `workers:\n  worker:\n    image: ${image("worker")}\n    command: npm run worker\n    dependsOn: [api, postgres]\n`
    : "";
  const dependencies = services.includes("api")
    ? "dependencies:\n  postgres: {}\n  redis: {}\nmigrations:\n  service: api\n  command: npm run migrate\n"
    : services.includes("search")
      ? "dependencies:\n  redis: {}\n"
      : "";
  return `version: 1\nservices:\n${declarations.join("\n")}\n${worker}${dependencies}smokeTests:\n  - name: homepage\n    command: curl -f http://${application[0]}:3000/\npreview:\n  visibility: private\n  ttl: 24h\n`;
}

export function createDemoEnvironments(): Environment[] {
  const now = Date.now();
  const at = (minutes: number) =>
    new Date(now + minutes * 60_000).toISOString();
  const make = (
    repository: string,
    pr: number,
    phase: string,
    branch: string,
    commit: string,
    minutesOld: number,
    ttlMinutes: number,
    services: string[],
  ): Environment => {
    const id = `${repository}-${pr}`;
    const expiresAt = at(ttlMinutes);
    return {
      id,
      tenantID: "demo",
      repositoryID: `repo-${repository}`,
      clusterID: "demo-eu-1",
      name: `${repository}-pr-${pr}`,
      version: 3,
      generation: 1,
      resetNonce: 0,
      desiredState: "Running",
      phase,
      expiresAt,
      createdAt: at(-minutesOld),
      updatedAt: at(-1),
      spec: {
        tenant: "demo",
        repository,
        pullRequest: pr,
        commit,
        branch,
        generation: 1,
        environmentID: id,
        owner: "demo-team",
        urlSuffix: "preview.example.com",
        expiresAt,
        desiredState: "Running",
        config: { inline: sampleConfig(services), sha256: "a".repeat(64) },
        images: Object.fromEntries(
          services.map((service) => [
            service,
            `ghcr.io/heimdall-demo/${service}@sha256:${"a".repeat(64)}`,
          ]),
        ),
      },
      status: {
        observedGeneration: 1,
        phase,
        namespace: `preview-${repository}-${pr}`,
        steps: [],
        urls: [],
      },
    };
  };
  const readySteps: NonNullable<EnvironmentStatus["steps"]> = [
    { name: "guardrails/setup", state: "succeeded", durationSeconds: 8 },
    { name: "dependencies/start", state: "succeeded", durationSeconds: 22 },
    { name: "baseline-db/prepare", state: "succeeded", durationSeconds: 3 },
    { name: "baseline-db/migrate", state: "succeeded", durationSeconds: 12 },
    { name: "baseline-db/clone", state: "succeeded", durationSeconds: 5 },
    { name: "application/wave-1", state: "succeeded", durationSeconds: 24 },
    { name: "smoke/run", state: "succeeded", durationSeconds: 7 },
  ];
  const shopReady = make(
    "shopflow",
    184,
    "Ready",
    "feat/checkout-redesign",
    "9f3a27b7198866baffbb7ea90e4baa29a77bb037",
    38,
    21 * 60,
    ["web", "api", "worker"],
  );
  shopReady.status = {
    ...shopReady.status,
    deployedGeneration: 1,
    steps: readySteps.map((step) => ({ ...step })),
    urls: [
      {
        service: "web",
        url: "https://shopflow-pr-184.preview.example.com",
        primary: true,
      },
      {
        service: "api",
        url: "https://api-shopflow-pr-184.preview.example.com",
      },
    ],
  };
  const shopFailed = make(
    "shopflow",
    185,
    "Failed",
    "feat/discount-rules",
    "c742ad20c2f09768606c53a3c5fa785c593c4c44",
    12,
    23 * 60,
    ["web", "api", "worker"],
  );
  shopFailed.status = {
    ...shopFailed.status,
    steps: [
      ...readySteps.slice(0, 3).map((step) => ({ ...step })),
      {
        name: "baseline-db/migrate",
        state: "failed",
        durationSeconds: 11,
        code: "engine.job_failed",
      },
    ],
    lastError: {
      code: "engine.job_failed",
      message: "Migration job exited with code 1.",
      step: "baseline-db/migrate",
      generation: 1,
      retryable: false,
      at: at(-3),
    },
    diagnoses: [
      {
        code: "MIGRATION_FAILED",
        summary: "The discount migration references a missing column.",
        suggestion:
          "Fix the migration to use the current schema, push the commit, and wait for the new preview generation.",
        evidence: [
          "Migration 20261005_add_discount_rules.sql exited with code 1.",
          'column "discount_type" does not exist',
        ],
        subject: "migrate",
        stage: "baseline-db/migrate",
      },
    ],
  };
  const storefront = make(
    "storefront",
    42,
    "Provisioning",
    "feat/product-search",
    "46ae815d12b26a24c8a31c0869bcbd31a020a481",
    2,
    24 * 60,
    ["web", "search"],
  );
  storefront.status.steps = [
    { name: "guardrails/setup", state: "succeeded", durationSeconds: 7 },
    { name: "dependencies/start", state: "succeeded", durationSeconds: 16 },
    { name: "application/wave-1", state: "running", startedAt: at(-1) },
  ];
  const docs = make(
    "docs-site",
    28,
    "Ready",
    "docs/getting-started",
    "2c1efb9637c888ca0e98a0e2f24ca46548dfbfab",
    23 * 60,
    42,
    ["docs"],
  );
  docs.status = {
    ...docs.status,
    deployedGeneration: 1,
    steps: [
      { name: "guardrails/setup", state: "succeeded", durationSeconds: 6 },
      { name: "application/wave-1", state: "succeeded", durationSeconds: 15 },
      { name: "smoke/run", state: "succeeded", durationSeconds: 4 },
    ],
    urls: [
      {
        service: "docs",
        url: "https://docs-site-pr-28.preview.example.com",
        primary: true,
      },
    ],
  };
  return [shopReady, shopFailed, storefront, docs];
}

export function createDemoEvents(env: Environment): Event[] {
  const start = Date.parse(env.createdAt);
  const events: Event[] = [
    {
      id: 1,
      environmentID: env.id,
      generation: env.generation,
      type: "environment.created",
      message: "Sample pull request opened. Preview intent accepted.",
      createdAt: env.createdAt,
    },
  ];
  for (const [index, step] of (env.status.steps ?? []).entries()) {
    events.push({
      id: index + 2,
      environmentID: env.id,
      generation: env.generation,
      type: `step.${step.state}`,
      message: `${step.name}: ${step.state}`,
      data: {
        step: step.name,
        state: step.state,
        ...(step.code ? { code: step.code } : {}),
      },
      createdAt: new Date(start + (index + 1) * 10_000).toISOString(),
    });
  }
  if ((env.status.steps ?? []).length === 0)
    events.push({
      id: 2,
      environmentID: env.id,
      generation: env.generation,
      type: "environment.queued",
      message:
        "Sample action accepted. An agent must reconcile the desired state.",
      createdAt: env.updatedAt,
    });
  return events;
}

export function demoLogs(workload: string): string {
  const now = Date.now();
  const at = (secondsAgo: number) =>
    new Date(now - secondsAgo * 1_000).toISOString();
  return `[sample logs · ${workload} · secrets redacted]\n${at(18)} INFO starting ${workload}\n${at(17)} INFO database connection established\n${at(17)} INFO HTTP server listening on :3000\n${at(13)} INFO GET /health 200 3ms\n${at(6)} INFO GET / 200 18ms\n`;
}

/** Simulates accepted desired-state changes only; no deployment or repair is performed. */
export function demoAction(
  env: Environment,
  action: ActionName,
  expiresAt?: string,
): Environment {
  const next = structuredClone(env);
  next.updatedAt = new Date().toISOString();
  next.version++;
  if (action === "extend") {
    if (
      !expiresAt ||
      !Number.isFinite(Date.parse(expiresAt)) ||
      Date.parse(expiresAt) <= Date.parse(env.expiresAt)
    )
      throw new APIError(
        "Choose an expiry after the current expiry.",
        400,
        "api.invalid_expiry",
      );
    next.expiresAt = expiresAt;
  } else {
    next.generation++;
    next.phase =
      action === "retry"
        ? "Pending"
        : action === "reset"
          ? "Resetting"
          : "Destroying";
    next.status = {};
    if (action === "reset") next.resetNonce++;
    if (action === "delete") next.desiredState = "Destroyed";
  }
  next.spec = {
    ...next.spec,
    generation: next.generation,
    resetNonce: next.resetNonce,
    desiredState: next.desiredState,
    expiresAt: next.expiresAt,
  };
  return next;
}
