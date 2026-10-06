import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  ArrowDownRight,
  ArrowRight,
  ArrowUpRight,
  BookOpen,
  Box,
  Check,
  ChevronDown,
  ChevronRight,
  Clock3,
  Copy,
  Database,
  ExternalLink,
  GitBranch,
  GitPullRequest,
  LayoutDashboard,
  LoaderCircle,
  LogOut,
  Menu,
  Network,
  Play,
  Plus,
  Radio,
  RefreshCw,
  Rocket,
  RotateCcw,
  Search,
  ShieldCheck,
  Terminal,
  Trash2,
  TriangleAlert,
  Workflow,
  X,
} from "lucide-react";
import {
  DashboardAPI,
  APIError,
  createDemoEnvironments,
  createDemoEvents,
  demoAction,
  demoLogs,
  environmentMeta,
  type ActionName,
  type Cluster,
  type Environment,
  type Event,
} from "./data";
import { LaunchDemo, Library, Onboarding } from "./Guide";
import "./dashboard.css";

type View = "overview" | "setup" | "docs" | "demo" | "connections";
type DetailTab = "Overview" | "Timeline" | "Logs";
function errorMessage(error: unknown) {
  const message =
    error instanceof Error
      ? error.message
      : "The request could not be completed.";
  return error instanceof APIError
    ? `${message} (${error.code}${error.requestID ? ` · request ${error.requestID}` : ""})`
    : message;
}
const navigation = [
  { view: "overview", label: "Environments", icon: LayoutDashboard },
  { view: "setup", label: "Getting started", icon: Rocket },
  { view: "connections", label: "Connections", icon: Network },
  { view: "docs", label: "Documentation", icon: BookOpen },
  { view: "demo", label: "Launch demo", icon: Play },
] as const;
const pipeline = [
  { name: "Guardrails", prefix: "guardrails/", icon: ShieldCheck },
  { name: "Dependencies", prefix: "dependencies/", icon: Database },
  { name: "Database", prefix: "baseline-db/", icon: Database },
  { name: "Application", prefix: "application/", icon: Box },
  { name: "Smoke tests", prefix: "smoke/", icon: Check },
];

function initialView(): View {
  const value = new URLSearchParams(window.location.search).get("view");
  return navigation.some((n) => n.view === value)
    ? (value as View)
    : "overview";
}
function timeLeft(date: string) {
  const hours = (Date.parse(date) - Date.now()) / 3600000;
  if (!Number.isFinite(hours)) return "Expiry unavailable";
  if (hours <= 0) return "Expiry passed";
  if (hours < 1) return `${Math.ceil(hours * 60)}m remaining`;
  if (hours >= 24)
    return `${Math.floor(hours / 24)}d ${Math.floor(hours % 24)}h remaining`;
  return `${Math.floor(hours)}h ${Math.floor((hours % 1) * 60)}m remaining`;
}
function relativeTime(date: string) {
  const minutes = Math.max(
    0,
    Math.floor((Date.now() - Date.parse(date)) / 60000),
  );
  if (!Number.isFinite(minutes)) return "Time unavailable";
  return minutes < 1
    ? "Just now"
    : minutes < 60
      ? `${minutes}m ago`
      : minutes < 1440
        ? `${Math.floor(minutes / 60)}h ago`
        : `${Math.floor(minutes / 1440)}d ago`;
}
function phaseClass(phase: string) {
  return phase === "Ready"
    ? "ready"
    : ["Failed", "Degraded"].includes(phase)
      ? "failed"
      : ["Destroyed", "Destroying"].includes(phase)
        ? "ended"
        : "running";
}
function safeURL(value: string | undefined) {
  if (!value) return null;
  try {
    const url = new URL(value);
    return ["http:", "https:"].includes(url.protocol) &&
      !url.username &&
      !url.password
      ? url.href
      : null;
  } catch {
    return null;
  }
}
function stageState(env: Environment, prefix: string) {
  const conditionName: Record<string, string> = {
    "guardrails/": "GuardrailsReady",
    "dependencies/": "DependenciesReady",
    "baseline-db/": "BaselineDatabaseReady",
    "application/": "ApplicationReady",
    "smoke/": "SmokeTestsPassed",
  };
  const condition = env.status.conditions?.find(
    (c) => c.type === conditionName[prefix],
  );
  if (condition?.status === "True") return "done";
  const steps =
    env.status.steps?.filter((s) => s.name.startsWith(prefix)) || [];
  if (!steps.length) return "waiting";
  if (steps.some((s) => s.state === "failed")) return "failed";
  if (steps.some((s) => ["running", "started"].includes(s.state)))
    return "running";
  if (condition) return "waiting";
  const stageIndex = pipeline.findIndex((s) => s.prefix === prefix);
  const hasLaterStep = pipeline
    .slice(stageIndex + 1)
    .some((stage) =>
      env.status.steps?.some((s) => s.name.startsWith(stage.prefix)),
    );
  const operationComplete =
    env.status.operation?.generation === env.generation &&
    env.status.operation.result === "Succeeded";
  return steps.every((s) => ["succeeded", "skipped"].includes(s.state)) &&
    (hasLaterStep ||
      operationComplete ||
      (env.phase === "Ready" &&
        env.status.deployedGeneration === env.generation))
    ? "done"
    : "waiting";
}
function StageStrip({
  env,
  compact = false,
}: {
  env: Environment;
  compact?: boolean;
}) {
  return (
    <div
      className={`fd-stage-strip ${compact ? "compact" : ""}`}
      aria-label="Deployment stages"
    >
      {pipeline.map((stage, index) => {
        const state = stageState(env, stage.prefix);
        return (
          <div
            key={stage.name}
            className={`fd-stage ${state}`}
            title={`${stage.name}: ${state}`}
          >
            <span>
              {state === "done" ? (
                <Check size={compact ? 10 : 15} />
              ) : state === "failed" ? (
                <X size={compact ? 10 : 15} />
              ) : state === "running" ? (
                <LoaderCircle size={compact ? 10 : 15} />
              ) : (
                <stage.icon size={compact ? 10 : 15} />
              )}
            </span>
            {!compact && <b>{stage.name}</b>}
            <i className="fd-sr-only">{`${index + 1}. ${stage.name}: ${state}`}</i>
          </div>
        );
      })}
    </div>
  );
}
function Status({ phase }: { phase: string }) {
  return (
    <span className={`fd-status ${phaseClass(phase)}`}>
      <i />
      {phase}
    </span>
  );
}
function timelineMessage(event: Event) {
  const details = [
    event.data?.step,
    event.data?.state,
    event.data?.phase,
    event.data?.action,
    event.data?.code,
  ].filter(
    (value): value is string => typeof value === "string" && Boolean(value),
  );
  return [
    event.message,
    ...details.filter((value) => !event.message.includes(value)),
    ...(typeof event.data?.durationSeconds === "number" &&
    Number.isFinite(event.data.durationSeconds)
      ? [`${event.data.durationSeconds}s`]
      : []),
  ].join(" · ");
}
function Dialog({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const previousFocus = useRef(document.activeElement as HTMLElement | null);
  useEffect(() => {
    const dialog = ref.current;
    dialog?.showModal();
    return () => {
      dialog?.close();
      previousFocus.current?.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      className="fd-dialog"
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      onKeyDown={(e) => {
        if (e.key !== "Tab") return;
        const targets = Array.from(
          ref.current?.querySelectorAll<HTMLElement>(
            'button:not([disabled]), input:not([disabled]), select:not([disabled]), a[href], [tabindex="0"]',
          ) || [],
        ).filter((element) => element.getClientRects().length > 0);
        const first = targets[0],
          last = targets.at(-1);
        if (e.shiftKey && document.activeElement === first) {
          e.preventDefault();
          last?.focus();
        } else if (!e.shiftKey && document.activeElement === last) {
          e.preventDefault();
          first?.focus();
        }
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
      aria-labelledby="fd-dialog-title"
    >
      <div className="fd-dialog-heading">
        <h2 id="fd-dialog-title">{title}</h2>
        <button className="fd-icon" onClick={onClose} aria-label="Close dialog">
          <X size={19} />
        </button>
      </div>
      {children}
    </dialog>
  );
}

function Inspector({
  env,
  sample,
  api,
  onAction,
  onClose,
  onDocs,
}: {
  env: Environment;
  sample: boolean;
  api: DashboardAPI | null;
  onAction: (action: ActionName) => void;
  onClose: () => void;
  onDocs: () => void;
}) {
  const [tab, setTab] = useState<DetailTab>("Overview");
  const [events, setEvents] = useState<Event[]>([]);
  const [eventError, setEventError] = useState("");
  const [loadingEvents, setLoadingEvents] = useState(false);
  const [logs, setLogs] = useState("");
  const [logError, setLogError] = useState("");
  const [loadingLogs, setLoadingLogs] = useState(false);
  const [workload, setWorkload] = useState("");
  const [logSequence, setLogSequence] = useState(0);
  const [copied, setCopied] = useState(false);
  const [eventSequence, setEventSequence] = useState(0);
  const panelRef = useRef<HTMLElement>(null);
  const savedFocus = useRef(document.activeElement as HTMLElement | null);
  const meta = environmentMeta(env);
  const diagnosis = env.status.diagnoses?.[0];
  const primaryURL = safeURL(
    env.status.urls?.find((u) => u.primary)?.url || env.status.urls?.[0]?.url,
  );
  const currentReady =
    env.phase === "Ready" && env.status.deployedGeneration === env.generation;
  const currentObservation =
    env.status.operation?.generation === env.generation ||
    env.status.deployedGeneration === env.generation;
  useEffect(() => {
    if (!window.matchMedia("(max-width: 1299px)").matches) return;
    panelRef.current?.querySelector<HTMLButtonElement>("button")?.focus();
    return () => savedFocus.current?.focus();
  }, []);
  useEffect(() => {
    if (!logs || sample) return;
    const expiryTimer = window.setTimeout(() => {
      setLogs("");
      setLogError("This log tail has expired. Request a fresh tail.");
    }, 90000);
    return () => window.clearTimeout(expiryTimer);
  }, [logs, sample]);
  useEffect(() => {
    setTab("Overview");
    setLogs("");
    setLogError("");
    setWorkload("");
    setCopied(false);
  }, [env.id, env.generation]);
  useEffect(() => {
    if (tab !== "Timeline") return;
    let active = true;
    setLoadingEvents(true);
    setEventError("");
    setEvents([]);
    const request = sample
      ? Promise.resolve(createDemoEvents(env))
      : api
        ? api.events(env.id)
        : Promise.resolve([]);
    request
      .then((result) => {
        if (active) setEvents(result);
      })
      .catch((error) => {
        if (active) setEventError(errorMessage(error));
      })
      .finally(() => {
        if (active) setLoadingEvents(false);
      });
    return () => {
      active = false;
    };
  }, [api, env.id, env.generation, env.version, sample, tab, eventSequence]);
  useEffect(() => {
    setLogs("");
    setLogError("");
    if (tab !== "Logs" || !workload) return;
    const controller = new AbortController();
    setLoadingLogs(true);
    const request = sample
      ? Promise.resolve(demoLogs(workload))
      : api
        ? api.logs(env, workload, controller.signal)
        : Promise.resolve("");
    request
      .then((result) => {
        if (!controller.signal.aborted) setLogs(result);
      })
      .catch((error) => {
        if (!controller.signal.aborted) setLogError(errorMessage(error));
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoadingLogs(false);
      });
    return () => {
      controller.abort();
    };
  }, [api, env.id, env.generation, sample, tab, workload, logSequence]);
  async function copyURL() {
    if (!primaryURL) return;
    try {
      await navigator.clipboard.writeText(primaryURL);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }
  return (
    <aside
      ref={panelRef}
      className="fd-inspector"
      aria-label="Environment details"
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.preventDefault();
          onClose();
        }
        if (
          event.key === "Tab" &&
          window.matchMedia("(max-width: 700px)").matches
        ) {
          const targets = Array.from(
            panelRef.current?.querySelectorAll<HTMLElement>(
              'button:not([disabled]), select, a[href], summary, [tabindex="0"]',
            ) || [],
          ).filter((element) => element.getClientRects().length > 0);
          if (event.shiftKey && document.activeElement === targets[0]) {
            event.preventDefault();
            targets.at(-1)?.focus();
          } else if (
            !event.shiftKey &&
            document.activeElement === targets.at(-1)
          ) {
            event.preventDefault();
            targets[0]?.focus();
          }
        }
      }}
    >
      <div className="fd-inspector-top">
        <span className="fd-kicker">
          PREVIEW / {meta.pr ? `#${meta.pr}` : env.id.slice(0, 8)}
        </span>
        <button
          className="fd-icon"
          onClick={onClose}
          aria-label="Close environment details"
        >
          <X size={18} />
        </button>
      </div>
      <div className="fd-inspector-title">
        <div className="fd-project-icon">
          <Box size={23} />
        </div>
        <div>
          <h2>{meta.repository}</h2>
          <span>
            <GitBranch size={13} />
            {meta.branch}
          </span>
        </div>
      </div>
      <div className="fd-inspector-meta">
        <Status phase={env.phase} />
        <span>Generation {env.generation}</span>
      </div>
      <div
        className="fd-detail-tabs"
        role="tablist"
        aria-label="Environment detail tabs"
      >
        {(["Overview", "Timeline", "Logs"] as const).map((t, index, all) => (
          <button
            key={t}
            role="tab"
            id={`fd-tab-${t}`}
            aria-controls={`fd-panel-${t}`}
            aria-selected={tab === t}
            tabIndex={tab === t ? 0 : -1}
            onClick={() => setTab(t)}
            onKeyDown={(e) => {
              if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) {
                e.preventDefault();
                const next =
                  e.key === "Home"
                    ? 0
                    : e.key === "End"
                      ? 2
                      : (index + (e.key === "ArrowRight" ? 1 : 2)) % 3;
                setTab(all[next]);
                document.getElementById(`fd-tab-${all[next]}`)?.focus();
              }
            }}
          >
            {t}
          </button>
        ))}
      </div>
      <div
        className="fd-detail-content"
        role="tabpanel"
        id={`fd-panel-${tab}`}
        aria-labelledby={`fd-tab-${tab}`}
      >
        {tab === "Overview" && (
          <>
            {(diagnosis || env.status.lastError) && (
              <div className="fd-diagnosis">
                <div>
                  <TriangleAlert size={18} />
                  <b>Let’s get this moving again.</b>
                </div>
                <code>{diagnosis?.code || env.status.lastError?.code}</code>
                <p>{diagnosis?.summary || env.status.lastError?.message}</p>
                {diagnosis?.suggestion && (
                  <p className="fd-suggestion">{diagnosis.suggestion}</p>
                )}
                {diagnosis?.evidence && (
                  <details>
                    <summary>View evidence</summary>
                    <pre>{diagnosis.evidence.join("\n")}</pre>
                  </details>
                )}
                <button onClick={onDocs} className="fd-text-link">
                  Troubleshooting guide <ArrowUpRight size={14} />
                </button>
              </div>
            )}
            <div className="fd-detail-section">
              <div className="fd-section-head">
                <h3>Deployment path</h3>
                <span>5 stages</span>
              </div>
              {!currentObservation && (
                <p className="fd-small">
                  Last observed stages. Waiting for this generation’s operation
                  report.
                </p>
              )}
              <StageStrip env={env} />
              {env.status.steps?.length ? (
                <div className="fd-step-summary">
                  {env.status.steps.map((s) => (
                    <div key={s.name}>
                      <span
                        className={s.state === "failed" ? "fd-error-text" : ""}
                      >
                        {s.name}
                      </span>
                      <b>{s.state}</b>
                    </div>
                  ))}
                </div>
              ) : (
                <p className="fd-small">
                  Waiting for the agent’s first stage report.
                </p>
              )}
            </div>
            <div className="fd-detail-section">
              <div className="fd-section-head">
                <h3>Services</h3>
                <span>{meta.services.length} declared</span>
              </div>
              {meta.services.length ? (
                meta.services.map((service) => (
                  <div key={service} className="fd-service">
                    <Box size={16} />
                    <b>{service}</b>
                    <span>Declared</span>
                    <button
                      className="fd-icon"
                      aria-label={`View ${service} logs`}
                      onClick={() => {
                        setWorkload(service);
                        setTab("Logs");
                      }}
                    >
                      <Terminal size={15} />
                    </button>
                  </div>
                ))
              ) : (
                <p className="fd-small">
                  Services appear after a build supplies a committed spec.
                </p>
              )}
            </div>
            <div className="fd-preview-url">
              <span className="fd-kicker">PREVIEW ADDRESS</span>
              {primaryURL ? (
                <>
                  <code>{new URL(primaryURL).host}</code>
                  <div>
                    {sample ? (
                      <span className="fd-small">
                        Illustrative address · no live preview
                      </span>
                    ) : currentReady ? (
                      <a
                        className="fd-text-link"
                        href={primaryURL}
                        target="_blank"
                        rel="noreferrer"
                      >
                        Open preview <ExternalLink size={14} />
                      </a>
                    ) : (
                      <span className="fd-small">
                        Waiting for this generation to be ready
                      </span>
                    )}
                    <button
                      className="fd-icon"
                      onClick={copyURL}
                      aria-label="Copy preview address"
                    >
                      <Copy size={14} />
                    </button>
                  </div>
                  <span role="status" className="fd-small">
                    {copied ? "Address copied" : ""}
                  </span>
                </>
              ) : (
                <p className="fd-small">
                  An address appears when reported by the agent.
                </p>
              )}
            </div>
            <dl className="fd-facts">
              <div>
                <dt>Deployed commit</dt>
                <dd>
                  <code>{meta.commit || "Unavailable"}</code>
                </dd>
              </div>
              <div>
                <dt>Configured visibility</dt>
                <dd>
                  <ShieldCheck size={13} />
                  {meta.visibility}
                </dd>
              </div>
              <div>
                <dt>Expires</dt>
                <dd>{timeLeft(env.expiresAt)}</dd>
              </div>
              <div>
                <dt>Cluster</dt>
                <dd>{env.clusterID}</dd>
              </div>
            </dl>
          </>
        )}
        {tab === "Timeline" && (
          <>
            <div className="fd-section-head">
              <h3>Deployment history</h3>
              <button
                className="fd-icon"
                aria-label="Refresh timeline"
                onClick={() => setEventSequence((v) => v + 1)}
              >
                <RefreshCw size={15} />
              </button>
            </div>
            <p className="fd-small">
              Durable events include their deployment generation.
            </p>
            {loadingEvents ? (
              <p role="status">Loading timeline…</p>
            ) : eventError ? (
              <p role="alert" className="fd-error-text">
                {eventError}
              </p>
            ) : !events.length ? (
              <p className="fd-small">No events reported yet.</p>
            ) : (
              <ol className="fd-timeline">
                {[...events].reverse().map((event) => (
                  <li key={event.id}>
                    <i />
                    <div>
                      <b>
                        {event.type.replaceAll("_", " ").replaceAll(".", " · ")}
                      </b>
                      <p>{timelineMessage(event)}</p>
                      <span>
                        Gen {event.generation} · {relativeTime(event.createdAt)}
                      </span>
                    </div>
                  </li>
                ))}
              </ol>
            )}
          </>
        )}
        {tab === "Logs" && (
          <>
            <h3>A closer look.</h3>
            <p className="fd-small">
              Request a redacted tail from an application service. Live logs
              expire after two minutes.
            </p>
            <div className="fd-log-controls">
              <label>
                Workload
                <select
                  aria-label="Log workload"
                  value={workload}
                  onChange={(e) => setWorkload(e.target.value)}
                >
                  <option value="">Choose a service</option>
                  {meta.services.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </label>
              <button
                className="fd-icon"
                aria-label="Refresh logs"
                disabled={!workload || loadingLogs}
                onClick={() => setLogSequence((v) => v + 1)}
              >
                <RefreshCw size={16} />
              </button>
            </div>
            {loadingLogs ? (
              <p role="status" className="fd-log-placeholder">
                Waiting for the agent…
              </p>
            ) : logError ? (
              <p role="alert" className="fd-error-text">
                {logError}
              </p>
            ) : logs ? (
              <pre className="fd-log-output" tabIndex={0}>
                {logs}
              </pre>
            ) : (
              <div className="fd-log-placeholder">
                <Terminal size={28} />
                <p>Choose a service to inspect its logs.</p>
              </div>
            )}
            <span className="fd-small">
              {sample
                ? "Sample logs · browser-only demonstration"
                : "100 lines maximum per request · fetched on demand"}
            </span>
          </>
        )}
      </div>
      <div className="fd-inspector-actions">
        <button
          className="fd-secondary"
          disabled={env.desiredState === "Destroyed"}
          onClick={() => onAction("retry")}
        >
          <RefreshCw size={14} />
          Retry
        </button>
        <button
          className="fd-secondary"
          disabled={
            !["Ready", "Failed", "Degraded"].includes(env.phase) ||
            env.desiredState === "Destroyed"
          }
          onClick={() => onAction("reset")}
        >
          <RotateCcw size={14} />
          Reset
        </button>
        <button
          className="fd-secondary"
          disabled={env.desiredState === "Destroyed"}
          onClick={() => onAction("extend")}
        >
          <Clock3 size={14} />
          Extend
        </button>
        <button
          className="fd-icon fd-delete"
          disabled={env.desiredState === "Destroyed"}
          aria-label="Delete environment"
          onClick={() => onAction("delete")}
        >
          <Trash2 size={16} />
        </button>
      </div>
    </aside>
  );
}

export default function Dashboard() {
  const [view, setView] = useState<View>(initialView);
  const [menuOpen, setMenuOpen] = useState(false);
  const [overlay, setOverlay] = useState(
    () => window.matchMedia("(max-width: 1299px)").matches,
  );
  const [focusSearch, setFocusSearch] = useState(false);
  const [sample, setSample] = useState(true);
  const [api, setAPI] = useState<DashboardAPI | null>(null);
  const [environments, setEnvironments] = useState<Environment[]>(
    createDemoEnvironments,
  );
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [selectedID, setSelectedID] = useState<string | null>(() =>
    new URLSearchParams(window.location.search).get("env"),
  );
  const [filter, setFilter] = useState("All previews");
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [updatedAt, setUpdatedAt] = useState<string | null>(null);
  const [connectOpen, setConnectOpen] = useState(false);
  const [token, setToken] = useState("");
  const [connectError, setConnectError] = useState("");
  const [connecting, setConnecting] = useState(false);
  const [action, setAction] = useState<ActionName | null>(null);
  const [actionEnvironment, setActionEnvironment] =
    useState<Environment | null>(null);
  const [acting, setActing] = useState(false);
  const [actionError, setActionError] = useState("");
  const [notice, setNotice] = useState("");
  const [expiry, setExpiry] = useState("");
  const session = useRef(0);
  const latestRefresh = useRef(0);
  const searchRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    const media = window.matchMedia("(max-width: 1299px)");
    const changed = () => setOverlay(media.matches);
    media.addEventListener("change", changed);
    return () => media.removeEventListener("change", changed);
  }, []);
  useEffect(() => {
    if (focusSearch && view === "overview") {
      searchRef.current?.focus();
      setFocusSearch(false);
    }
  }, [focusSearch, view]);
  const selected = environments.find((e) => e.id === selectedID);
  const active = environments.filter((e) => e.desiredState !== "Destroyed");
  const ready = active.filter((e) => e.phase === "Ready").length;
  const attention = active.filter((e) =>
    ["Failed", "Degraded"].includes(e.phase),
  );
  const deploying = active.filter(
    (e) =>
      !["Ready", "Failed", "Degraded", "Destroyed", "Destroying"].includes(
        e.phase,
      ),
  ).length;
  const shown = environments.filter((env) => {
    const meta = environmentMeta(env);
    const matches =
      `${meta.repository} ${meta.branch} ${meta.pr || ""} ${env.name} ${env.id}`
        .toLowerCase()
        .includes(query.toLowerCase());
    return (
      matches &&
      (filter === "All previews" ||
        (filter === "Needs attention"
          ? ["Failed", "Degraded"].includes(env.phase)
          : filter === "Deploying"
            ? ![
                "Ready",
                "Failed",
                "Degraded",
                "Destroyed",
                "Destroying",
              ].includes(env.phase)
            : env.phase === filter))
    );
  });
  function navigate(next: View) {
    setView(next);
    setMenuOpen(false);
    const params = new URLSearchParams();
    if (next !== "overview") params.set("view", next);
    if (selectedID) params.set("env", selectedID);
    window.history.pushState(
      {},
      "",
      `/dashboard${params.size ? `?${params}` : ""}`,
    );
  }
  function selectEnvironment(id: string | null) {
    setSelectedID(id);
    const params = new URLSearchParams(window.location.search);
    if (id) params.set("env", id);
    else params.delete("env");
    window.history.replaceState(
      {},
      "",
      `/dashboard${params.size ? `?${params}` : ""}`,
    );
  }
  useEffect(() => {
    const pop = () => {
      setView(initialView());
      setSelectedID(new URLSearchParams(window.location.search).get("env"));
    };
    window.addEventListener("popstate", pop);
    return () => window.removeEventListener("popstate", pop);
  }, []);
  useEffect(() => {
    document.title = `Heimdall · ${navigation.find((n) => n.view === view)?.label}`;
  }, [view]);
  useEffect(() => {
    const shortcut = (e: KeyboardEvent) => {
      if ((e.ctrlKey || e.metaKey) && e.key === "k") {
        e.preventDefault();
        if (connectOpen || action) return;
        selectEnvironment(null);
        navigate("overview");
        setFocusSearch(true);
      }
    };
    window.addEventListener("keydown", shortcut);
    return () => window.removeEventListener("keydown", shortcut);
  });
  const refresh = useCallback(async () => {
    if (!api || sample) return;
    const snapshot = session.current;
    const request = ++latestRefresh.current;
    setLoading(true);
    try {
      const [envs, connections] = await Promise.all([
        api.environments(),
        api.clusters(),
      ]);
      if (snapshot !== session.current || request !== latestRefresh.current)
        return;
      setEnvironments(envs);
      setClusters(connections);
      setError("");
      setUpdatedAt(new Date().toISOString());
    } catch (e) {
      if (snapshot === session.current && request === latestRefresh.current)
        setError(
          e instanceof Error
            ? e.message
            : "The control plane could not be reached.",
        );
    } finally {
      if (snapshot === session.current && request === latestRefresh.current)
        setLoading(false);
    }
  }, [api, sample]);
  useEffect(() => {
    if (!api || sample) return;
    const timer = window.setInterval(() => {
      if (!document.hidden) void refresh();
    }, 15000);
    return () => window.clearInterval(timer);
  }, [api, sample, refresh]);
  useEffect(() => {
    if (!notice) return;
    const timer = window.setTimeout(() => setNotice(""), 6000);
    return () => window.clearTimeout(timer);
  }, [notice]);
  function returnToSample() {
    session.current++;
    setAPI(null);
    setSample(true);
    setEnvironments(createDemoEnvironments());
    setClusters([]);
    setError("");
    setUpdatedAt(null);
    setLoading(false);
    selectEnvironment(null);
    setNotice(
      "Sample workspace restored. The live session has been disconnected.",
    );
  }
  async function connect() {
    const nextAPI = new DashboardAPI(token.trim());
    const snapshot = ++session.current;
    setConnecting(true);
    setConnectError("");
    try {
      const [envs, connections] = await Promise.all([
        nextAPI.environments(),
        nextAPI.clusters(),
      ]);
      if (snapshot !== session.current) return;
      setAPI(nextAPI);
      setSample(false);
      setEnvironments(envs);
      setClusters(connections);
      setUpdatedAt(new Date().toISOString());
      setError("");
      selectEnvironment(null);
      setConnectOpen(false);
      setToken("");
      setNotice("Connected to the control plane.");
    } catch (e) {
      if (snapshot === session.current) setConnectError(errorMessage(e));
    } finally {
      if (snapshot === session.current) setConnecting(false);
    }
  }
  function cancelConnect() {
    session.current++;
    setConnectOpen(false);
    setToken("");
    setConnectError("");
    setConnecting(false);
  }
  function requestAction(next: ActionName) {
    if (!selected) return;
    setActionEnvironment(selected);
    setAction(next);
    setActionError("");
    const date = new Date(
      Math.max(Date.now(), Date.parse(selected.expiresAt)) + 24 * 3600000,
    );
    setExpiry(date.toISOString());
  }
  async function performAction() {
    if (!actionEnvironment || !action) return;
    const captured = actionEnvironment;
    const requested = action;
    const snapshot = session.current;
    setActing(true);
    setActionError("");
    try {
      const result = sample
        ? demoAction(
            captured,
            requested,
            requested === "extend" ? expiry : undefined,
          )
        : await api!.act(
            captured,
            requested,
            requested === "extend" ? expiry : undefined,
          );
      if (snapshot !== session.current) return;
      // An in-flight refresh must not overwrite the newer action response.
      latestRefresh.current++;
      setLoading(false);
      setEnvironments((current) =>
        current.map((env) => (env.id === result.id ? result : env)),
      );
      setAction(null);
      setNotice(
        sample
          ? `Sample ${requested} applied. No infrastructure was changed.`
          : `${requested[0].toUpperCase() + requested.slice(1)} accepted. Waiting for agent observations.`,
      );
    } catch (e) {
      if (snapshot === session.current) setActionError(errorMessage(e));
    } finally {
      if (snapshot === session.current) setActing(false);
    }
  }
  const title =
    view === "overview"
      ? "Your next change, in view."
      : view === "setup"
        ? "Make yourself at home."
        : view === "connections"
          ? "Everything, connected."
          : view === "docs"
            ? "The field guide."
            : "A preview worth showing.";
  return (
    <div className="flight-deck">
      <a className="fd-skip" href="#fd-main">
        Skip to content
      </a>
      <button
        className="fd-mobile-toggle fd-icon"
        style={{
          visibility:
            overlay && selected && view === "overview" ? "hidden" : undefined,
        }}
        aria-label={menuOpen ? "Close navigation" : "Open navigation"}
        onClick={() => setMenuOpen(!menuOpen)}
        aria-expanded={menuOpen}
        aria-controls="fd-navigation"
      >
        {menuOpen ? <X size={20} /> : <Menu size={20} />}
      </button>
      <aside
        className={`fd-sidebar ${menuOpen ? "open" : ""}`}
        id="fd-navigation"
      >
        <a className="fd-brand" href="/">
          <span>
            <img src="/images/heimdall-logo.jpg" alt="" />
          </span>
          heimdall<span className="fd-brand-dot">.</span>
        </a>
        <div className="fd-workspace">
          <div>H</div>
          <span>
            <b>{sample ? "The playground" : "Your workspace"}</b>
            <small>
              {sample ? "Explore with sample data" : "Control plane connected"}
            </small>
          </span>
          <ChevronDown size={14} />
        </div>
        <span className="fd-nav-label">WORKSPACE</span>
        <nav aria-label="Dashboard navigation">
          {navigation.map((n) => (
            <button
              key={n.view}
              onClick={() => navigate(n.view)}
              className={view === n.view ? "active" : ""}
              aria-current={view === n.view ? "page" : undefined}
            >
              <n.icon size={18} />
              {n.label}
              {n.view === "overview" && <span>{active.length}</span>}
            </button>
          ))}
        </nav>
        <div className="fd-sidebar-note">
          <span className="fd-note-symbol">
            <Workflow size={24} />
          </span>
          <b>
            A whole stack.
            <br />
            One small change.
          </b>
          <p>
            Let the preview do
            <br />
            the explaining.
          </p>
          <button onClick={() => navigate("demo")}>
            Take the tour <ArrowUpRight size={14} />
          </button>
        </div>
        <div className="fd-sidebar-bottom">
          <button
            onClick={() => (sample ? setConnectOpen(true) : returnToSample())}
          >
            <span className="fd-avatar">
              {sample ? <Plus size={16} /> : <LogOut size={16} />}
            </span>
            <span>
              <b>{sample ? "Connect your workspace" : "Disconnect session"}</b>
              <small>
                {sample ? "Use your control plane" : "Return to sample data"}
              </small>
            </span>
            <ArrowRight size={15} />
          </button>
        </div>
      </aside>
      <div className="fd-shell">
        <header className="fd-topbar">
          <div>
            <span>Workspace</span>
            <ChevronRight size={13} />
            <b>{navigation.find((n) => n.view === view)?.label}</b>
          </div>
          <div>
            <span className={`fd-mode-pill ${sample ? "" : "live"}`}>
              <i />
              {sample ? "Sample workspace" : "Live API"}
            </span>
            <button className="fd-top-docs" onClick={() => navigate("docs")}>
              <BookOpen size={16} /> Help
            </button>
            <span className="fd-user">H</span>
          </div>
        </header>
        <main
          id="fd-main"
          className={`fd-main ${selected && view === "overview" ? "has-inspector" : ""}`}
        >
          <div
            className="fd-page"
            inert={Boolean(overlay && selected && view === "overview")}
          >
            <div className="fd-page-heading">
              <div>
                <span className="fd-kicker">
                  {view === "overview"
                    ? "THE PREVIEW FLIGHT DECK"
                    : "HEIMDALL / YOUR WORKSPACE"}
                </span>
                <h1>{title}</h1>
                <p>
                  {view === "overview"
                    ? "A place for every pull request. A clear path for every deployment."
                    : "A little guidance. A lot more confidence in the next deployment."}
                </p>
              </div>
              {view === "overview" && (
                <button
                  className="fd-primary"
                  onClick={() => navigate("setup")}
                >
                  <Plus size={16} /> Set up a preview
                </button>
              )}
            </div>
            {error && (
              <div className="fd-error-banner" role="alert">
                <TriangleAlert size={18} />
                <div>
                  <b>We couldn’t refresh your workspace.</b>
                  <p>
                    {error}{" "}
                    {updatedAt ? "Showing the last successful snapshot." : ""}
                  </p>
                </div>
                <button className="fd-secondary" onClick={() => void refresh()}>
                  Try again
                </button>
              </div>
            )}
            {view === "overview" && (
              <>
                <section className="fd-runway" aria-label="Workspace snapshot">
                  <div className="fd-runway-copy">
                    <span className="fd-kicker">
                      YOUR WORK, WITHOUT THE WAIT
                    </span>
                    <h2>
                      Good ideas need <em>room to run.</em>
                    </h2>
                    <p>
                      {sample
                        ? "Explore a working interface with four illustrative previews. Connect your API when you’re ready."
                        : "Follow your previews from the first guardrail to the final smoke test."}
                    </p>
                    <button
                      onClick={() =>
                        sample ? setConnectOpen(true) : void refresh()
                      }
                      className="fd-text-link"
                    >
                      {sample
                        ? "Connect your control plane"
                        : "Refresh workspace"}
                      {loading ? (
                        <LoaderCircle size={15} className="fd-spin" />
                      ) : (
                        <ArrowUpRight size={15} />
                      )}
                    </button>
                  </div>
                  <div className="fd-runway-art" aria-hidden="true">
                    <div className="fd-orbit orbit-1" />
                    <div className="fd-orbit orbit-2" />
                    <div className="fd-orbit orbit-3" />
                    <div className="fd-orbit-core">
                      <Workflow size={36} strokeWidth={1.3} />
                    </div>
                    <span className="fd-floating node-1">
                      <GitPullRequest size={15} />
                      Your change
                    </span>
                    <span className="fd-floating node-2">
                      <ShieldCheck size={15} />
                      Guardrails
                    </span>
                    <span className="fd-floating node-3">
                      <Check size={15} />A world of its own
                    </span>
                  </div>
                </section>
                <section className="fd-metrics" aria-label="Preview summary">
                  <div>
                    <span>
                      Active previews <Box size={15} />
                    </span>
                    <b>{active.length.toString().padStart(2, "0")}</b>
                    <small>{ready} ready to review</small>
                  </div>
                  <div>
                    <span>
                      On the runway <Workflow size={15} />
                    </span>
                    <b>{deploying.toString().padStart(2, "0")}</b>
                    <small>Deployment in progress</small>
                  </div>
                  <div className={attention.length ? "attention" : ""}>
                    <span>
                      Needs a hand <TriangleAlert size={15} />
                    </span>
                    <b>{attention.length.toString().padStart(2, "0")}</b>
                    <small>
                      {attention.length
                        ? "A diagnosis is waiting"
                        : "No failures reported"}
                    </small>
                  </div>
                  <div>
                    <span>
                      Allocation estimate <ArrowDownRight size={15} />
                    </span>
                    <b>
                      {sample ? "$0.18" : "—"}
                      <small>{sample ? "/ hr" : ""}</small>
                    </b>
                    <small>
                      {sample
                        ? "Sample estimate · not a bill"
                        : "Cost data not available"}
                    </small>
                  </div>
                </section>
                {attention.length > 0 && (
                  <button
                    className="fd-attention-banner"
                    onClick={() => {
                      setFilter("Needs attention");
                      selectEnvironment(attention[0].id);
                    }}
                  >
                    <span>
                      <TriangleAlert size={19} />
                    </span>
                    <div>
                      <b>
                        {attention.length === 1
                          ? "One preview needs a little attention."
                          : `${attention.length} previews need a little attention.`}
                      </b>
                      <p>
                        {environmentMeta(attention[0]).repository}{" "}
                        {environmentMeta(attention[0]).pr
                          ? `#${environmentMeta(attention[0]).pr}`
                          : ""}{" "}
                        ·{" "}
                        {attention[0].status.diagnoses?.[0]?.code ||
                          attention[0].phase}
                      </p>
                    </div>
                    <span className="fd-text-link">
                      See diagnosis <ArrowRight size={15} />
                    </span>
                  </button>
                )}
                <section className="fd-board" aria-label="Environments">
                  <div className="fd-section-head">
                    <div>
                      <h2>
                        Your environments <span>{environments.length}</span>
                      </h2>
                      <p>Small worlds. Big possibilities.</p>
                    </div>
                    <button
                      className="fd-icon"
                      onClick={() =>
                        sample
                          ? setNotice(
                              "Sample data is local. Connect your control plane for live updates.",
                            )
                          : void refresh()
                      }
                      aria-label="Refresh environments"
                      disabled={loading}
                    >
                      <RefreshCw
                        size={17}
                        className={loading ? "fd-spin" : ""}
                      />
                    </button>
                  </div>
                  <div className="fd-board-tools">
                    <div
                      className="fd-filters"
                      role="group"
                      aria-label="Filter environments"
                    >
                      {[
                        "All previews",
                        "Ready",
                        "Deploying",
                        "Needs attention",
                        "Destroyed",
                      ].map((f) => (
                        <button
                          key={f}
                          className={filter === f ? "active" : ""}
                          aria-pressed={filter === f}
                          onClick={() => setFilter(f)}
                        >
                          {f}
                        </button>
                      ))}
                    </div>
                    <label className="fd-search">
                      <Search size={16} />
                      <input
                        ref={searchRef}
                        aria-label="Search environments"
                        placeholder="Search previews…"
                        value={query}
                        onChange={(e) => setQuery(e.target.value)}
                      />
                      <kbd>⌘ K</kbd>
                    </label>
                  </div>
                  <div className="fd-table-head" aria-hidden="true">
                    <span>ENVIRONMENT</span>
                    <span>DEPLOYMENT</span>
                    <span>EXPIRES</span>
                    <span />
                  </div>
                  <div className="fd-environment-list">
                    {shown.map((env) => {
                      const meta = environmentMeta(env);
                      return (
                        <button
                          key={env.id}
                          className={`fd-environment ${selectedID === env.id ? "selected" : ""}`}
                          onClick={() => selectEnvironment(env.id)}
                          aria-label={`Inspect ${meta.repository}${meta.pr ? ` #${meta.pr}` : ` ${env.name}`}`}
                          aria-pressed={selectedID === env.id}
                        >
                          <div
                            className={`fd-env-symbol ${phaseClass(env.phase)}`}
                          >
                            <GitPullRequest size={20} />
                          </div>
                          <div className="fd-env-name">
                            <b>
                              {meta.repository}
                              <span>{meta.pr ? `#${meta.pr}` : env.name}</span>
                            </b>
                            <small>
                              <GitBranch size={12} />
                              {meta.branch}
                            </small>
                          </div>
                          <div className="fd-env-deployment">
                            <Status phase={env.phase} />
                            <StageStrip env={env} compact />
                          </div>
                          <div className="fd-env-expiry">
                            <span>
                              {env.desiredState === "Destroyed"
                                ? "Ended"
                                : timeLeft(env.expiresAt)}
                            </span>
                            <small>Updated {relativeTime(env.updatedAt)}</small>
                          </div>
                          <ChevronRight size={16} />
                        </button>
                      );
                    })}
                  </div>
                  {!shown.length && (
                    <div className="fd-empty">
                      <Box size={30} />
                      <h3>
                        {environments.length
                          ? "No previews match your view."
                          : "Your first preview starts here."}
                      </h3>
                      <p>
                        {environments.length
                          ? "Try another filter or search term."
                          : "Follow the setup guide, then open a pull request in your connected repository."}
                      </p>
                      <button
                        className="fd-secondary"
                        onClick={() =>
                          environments.length
                            ? (setQuery(""), setFilter("All previews"))
                            : navigate("setup")
                        }
                      >
                        {environments.length ? "Clear filters" : "Start setup"}
                        <ArrowRight size={14} />
                      </button>
                    </div>
                  )}
                  <div className="fd-board-footer">
                    <span>
                      <Radio size={13} />
                      {sample
                        ? "Illustrative data · local to this browser"
                        : loading
                          ? "Refreshing…"
                          : updatedAt
                            ? `Last synced ${relativeTime(updatedAt)} · refreshes every 15s`
                            : "Not yet synced"}
                    </span>
                    <span>
                      Showing {shown.length} of {environments.length}
                    </span>
                  </div>
                </section>
                <div className="fd-bottom-note">
                  <ShieldCheck size={15} />
                  <p>
                    {sample
                      ? "Sample allocation: 1.5 vCPU × $0.06 + 3 GiB × $0.03 = $0.18/hr. Illustrative rates; excludes storage and shared infrastructure."
                      : "Configured visibility reflects the spec. Gateway access and isolation must be verified by the operator."}
                  </p>
                </div>
              </>
            )}
            {view === "setup" && <Onboarding />}
            {view === "docs" && <Library />}
            {view === "demo" && <LaunchDemo />}
            {view === "connections" && (
              <div className="fd-connections">
                <section className="fd-connection-hero">
                  <Network size={36} />
                  <h2>A bridge to your infrastructure.</h2>
                  <p>
                    {sample
                      ? "The playground doesn’t connect to a cluster. Use an issued tenant token to see your real environments and agent heartbeats."
                      : "The agent connects outbound from your cluster. This view reports the last heartbeat recorded by the control plane."}
                  </p>
                  <button
                    className="fd-primary"
                    onClick={() =>
                      sample ? setConnectOpen(true) : void refresh()
                    }
                  >
                    {sample ? "Connect control plane" : "Refresh connections"}
                    <ArrowRight size={15} />
                  </button>
                </section>
                {!sample && (
                  <section className="fd-clusters">
                    <div className="fd-section-head">
                      <h3>Customer clusters</h3>
                      <span>{clusters.length} registered</span>
                    </div>
                    {!clusters.length ? (
                      <p>
                        No clusters registered. Follow the enrollment guide to
                        add your first agent.
                      </p>
                    ) : (
                      clusters.map((cluster) => (
                        <div className="fd-cluster" key={cluster.id}>
                          <Network size={24} />
                          <div>
                            <b>{cluster.name}</b>
                            <span>
                              {cluster.id} · Tier {cluster.tier}
                            </span>
                          </div>
                          <span className="fd-heartbeat">
                            {cluster.lastHeartbeat
                              ? `Last seen ${relativeTime(cluster.lastHeartbeat)}`
                              : "No heartbeat yet"}
                          </span>
                        </div>
                      ))
                    )}
                  </section>
                )}
                <div className="fd-connection-guides">
                  <article>
                    <GitBranch size={23} />
                    <h3>GitHub App</h3>
                    <p>
                      App installation and repository registration are operator
                      steps. They aren’t verified by this dashboard.
                    </p>
                    <a
                      className="fd-text-link"
                      href="/docs/github-app-setup.md"
                      target="_blank"
                      rel="noreferrer"
                    >
                      Installation guide <ArrowUpRight size={14} />
                    </a>
                  </article>
                  <article>
                    <ShieldCheck size={23} />
                    <h3>Cluster enrollment</h3>
                    <p>
                      Install a reviewed agent using the Helm chart and a
                      one-use enrollment token. Keep enrollment secrets out of
                      the browser.
                    </p>
                    <a
                      className="fd-text-link"
                      href="/docs/control-plane.md"
                      target="_blank"
                      rel="noreferrer"
                    >
                      Agent setup guide <ArrowUpRight size={14} />
                    </a>
                  </article>
                </div>
              </div>
            )}
            <footer className="fd-page-footer">
              <span>
                HEIMDALL <i /> THE GUARDIAN OF YOUR NEXT CHANGE
              </span>
              <a
                href="/docs/dashboard-design.md"
                target="_blank"
                rel="noreferrer"
              >
                P11 design blueprint <ArrowUpRight size={12} />
              </a>
            </footer>
          </div>
          {view === "overview" && selected && (
            <Inspector
              key={selected.id}
              env={selected}
              sample={sample}
              api={api}
              onAction={requestAction}
              onClose={() => selectEnvironment(null)}
              onDocs={() => navigate("docs")}
            />
          )}
        </main>
      </div>
      <div
        className={`fd-toast ${notice ? "visible" : ""}`}
        role="status"
        aria-live="polite"
      >
        {notice && (
          <>
            <Check size={17} />
            {notice}
          </>
        )}
      </div>
      {connectOpen && (
        <Dialog title="Connect your control plane" onClose={cancelConnect}>
          <p>
            Use a tenant bearer token issued by your operator. The dashboard
            talks to the same-origin <code>/v1</code> API. Your token stays in
            memory and is cleared when you disconnect or reload.
          </p>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void connect();
            }}
          >
            <label className="fd-field">
              Tenant token
              <input
                type="password"
                name="tenant-token"
                autoComplete="off"
                value={token}
                onChange={(e) => setToken(e.target.value)}
                placeholder="Paste your issued token"
                required
              />
            </label>
            <p className="fd-small">
              Local development connects through the frontend proxy to port
              8080. For hosted use, serve the API behind the dashboard’s HTTPS
              origin.
            </p>
            {connectError && (
              <div className="fd-form-error" role="alert">
                {connectError}
              </div>
            )}
            <div className="fd-dialog-actions">
              <button
                type="button"
                className="fd-secondary"
                onClick={cancelConnect}
              >
                Cancel
              </button>
              <button
                className="fd-primary"
                disabled={connecting || !token.trim()}
              >
                {connecting ? "Connecting…" : "Connect workspace"}
                <ArrowRight size={15} />
              </button>
            </div>
          </form>
        </Dialog>
      )}
      {action && actionEnvironment && (
        <Dialog
          title={
            action === "delete"
              ? "Delete this environment?"
              : action === "reset"
                ? "Reset the preview data?"
                : action === "extend"
                  ? "Give this preview more time."
                  : "Retry this deployment?"
          }
          onClose={() => {
            if (!acting) setAction(null);
          }}
        >
          <p>
            <strong>
              {environmentMeta(actionEnvironment).repository}{" "}
              {environmentMeta(actionEnvironment).pr
                ? `#${environmentMeta(actionEnvironment).pr}`
                : actionEnvironment.name}
            </strong>{" "}
            · generation {actionEnvironment.generation} · version{" "}
            {actionEnvironment.version}
          </p>
          <p className="fd-action-explainer">
            {action === "delete"
              ? "Workloads, live preview data and the namespace will be removed. Deployment and audit history are retained."
              : action === "reset"
                ? "Application workloads pause while the live database is recreated from its baseline. Preview data changes will be lost, and services may be unavailable during the reset."
                : action === "extend"
                  ? "Change the expiry without restarting your preview. The server checks the tenant’s TTL limits."
                  : "Retry the committed deployment as a new generation. A broken migration still requires a code fix."}
          </p>
          {sample && (
            <span className="fd-mode-pill">
              Sample action · no infrastructure changes
            </span>
          )}
          <form
            onSubmit={(e) => {
              e.preventDefault();
              void performAction();
            }}
          >
            {action === "extend" && (
              <label className="fd-field">
                New expiry (UTC)
                <input
                  value={expiry}
                  onChange={(e) => setExpiry(e.target.value)}
                  required
                  placeholder="2026-10-07T12:00:00.000Z"
                />
                <span>Use an ISO date and time with timezone.</span>
              </label>
            )}
            {actionError && (
              <div role="alert" className="fd-form-error">
                {actionError}
                <button
                  type="button"
                  className="fd-text-link"
                  onClick={() => {
                    setAction(null);
                    void refresh();
                  }}
                >
                  Refresh environment and review <RefreshCw size={14} />
                </button>
              </div>
            )}
            <div className="fd-dialog-actions">
              <button
                type="button"
                className="fd-secondary"
                disabled={acting}
                onClick={() => setAction(null)}
              >
                Cancel
              </button>
              <button
                className={action === "delete" ? "fd-danger" : "fd-primary"}
                disabled={acting || Boolean(actionError)}
              >
                {acting
                  ? "Submitting…"
                  : `${sample ? "Simulate" : "Confirm"} ${action}`}
                <ArrowRight size={15} />
              </button>
            </div>
          </form>
        </Dialog>
      )}
    </div>
  );
}
