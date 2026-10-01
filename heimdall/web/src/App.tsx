import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  ArrowDown,
  ArrowRight,
  ArrowUpRight,
  Blocks,
  BookOpen,
  Box,
  Check,
  CheckCheck,
  ChevronDown,
  Code2,
  Copy,
  Database,
  FileCode2,
  GitBranch,
  GitPullRequest,
  Layers3,
  LockKeyhole,
  Menu,
  Network,
  Play,
  RotateCcw,
  ShieldCheck,
  Sparkles,
  Terminal,
  Workflow,
  X,
} from "lucide-react";

const stages = [
  {
    name: "Guardrails",
    detail: "Namespace, quotas & network policy",
    time: "01",
  },
  { name: "Dependencies", detail: "PostgreSQL, Redis & RabbitMQ", time: "02" },
  { name: "Baseline database", detail: "Migrate, seed & clone", time: "03" },
  {
    name: "Application",
    detail: "Services & workers, in dependency order",
    time: "04",
  },
  { name: "Smoke tests", detail: "Verify the stack is ready", time: "05" },
];
const technologies = [
  { name: "Kubernetes", icon: Network },
  { name: "PostgreSQL", icon: Database },
  { name: "Redis", icon: Layers3 },
  { name: "RabbitMQ", icon: Workflow },
  { name: "Docker", icon: Box },
];
const config = `version: 1

services:
  web:
    build: {context: web}
    port: 3000
    health: {path: /}
    public: true
    primary: true
    dependsOn: [api]
  api:
    build: {context: api}
    port: 8080
    health: {path: /health}
    dependsOn: [postgres, redis]

dependencies:
  postgres: {version: "16", storage: 2Gi}
  redis: {}

preview:
  ttl: 48h
  visibility: private`;
const commands = `# Run from the Heimdall repository. Requires Go 1.26+.
go build -o bin/heimdall ./cmd/heimdall

# Check the included full-stack example
./bin/heimdall validate examples/shopflow/heimdall.yaml

# Inspect the deployment plan without cluster access
./bin/heimdall render --placeholder-images --list examples/shopflow/heimdall.yaml

# Export staged Kubernetes manifests
./bin/heimdall render --placeholder-images --out-dir out/preview examples/shopflow/heimdall.yaml`;

function Brand({ footer = false }: { footer?: boolean }) {
  return (
    <a
      className={`brand ${footer ? "brand-footer" : ""}`}
      href="#top"
      aria-label="Heimdall home"
    >
      <span className="brand-mark">
        <img src="/images/heimdall-logo.jpg" alt="" width="48" height="48" />
      </span>
      <span>
        heimdall<span className="brand-period">.</span>
      </span>
    </a>
  );
}

function Modal({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    ref.current?.showModal();
    const previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previous;
    };
  }, []);
  return (
    <dialog
      ref={ref}
      className="modal glass"
      onClose={onClose}
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div className="modal-heading">
        <div>
          <span className="eyebrow">HEIMDALL / EXPLORE</span>
          <h2>{title}</h2>
        </div>
        <button
          className="icon-button"
          onClick={onClose}
          aria-label="Close dialog"
        >
          <X size={20} />
        </button>
      </div>
      {children}
    </dialog>
  );
}

function Demo({ onClose }: { onClose: () => void }) {
  const [tab, setTab] = useState("Pipeline");
  const [progress, setProgress] = useState(5);
  useEffect(() => {
    if (progress >= 5) return;
    const timer = window.setTimeout(() => setProgress((p) => p + 1), 850);
    return () => window.clearTimeout(timer);
  }, [progress]);
  return (
    <Modal title="A whole stack. One preview." onClose={onClose}>
      <p className="modal-intro">
        An interactive walkthrough of the ShopFlow deployment plan. This
        simulation runs locally in your browser.
      </p>
      <div className="demo-top">
        <span>
          <GitPullRequest size={18} /> shopflow <b>#184</b>
        </span>
        <span className={`status-pill ${progress < 5 ? "running" : ""}`}>
          <span className="status-dot" />
          {progress < 5 ? "Deploying" : "Preview ready"}
        </span>
      </div>
      <div className="tabs" role="tablist" aria-label="Preview walkthrough">
        {["Pipeline", "Services", "Logs"].map((t) => (
          <button
            key={t}
            id={`demo-tab-${t}`}
            role="tab"
            aria-controls={`demo-panel-${t}`}
            aria-selected={tab === t}
            onClick={() => setTab(t)}
            onKeyDown={(e) => {
              const names = ["Pipeline", "Services", "Logs"];
              if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
                e.preventDefault();
                const next =
                  names[
                    (names.indexOf(t) + (e.key === "ArrowRight" ? 1 : 2)) % 3
                  ];
                setTab(next);
                document.getElementById(`demo-tab-${next}`)?.focus();
              }
            }}
          >
            {t}
          </button>
        ))}
      </div>
      <div
        role="tabpanel"
        id={`demo-panel-${tab}`}
        aria-labelledby={`demo-tab-${tab}`}
        className="demo-panel"
      >
        {tab === "Pipeline" &&
          stages.map((s, i) => (
            <div
              className={`demo-stage ${i === progress ? "current" : ""}`}
              key={s.name}
            >
              <span className={`stage-check ${i < progress ? "complete" : ""}`}>
                {i < progress ? <Check size={14} /> : i + 1}
              </span>
              <div>
                <b>{s.name}</b>
                <p>{s.detail}</p>
              </div>
              <span className="stage-result">
                {i < progress
                  ? "Complete"
                  : i === progress
                    ? "Running…"
                    : "Queued"}
              </span>
            </div>
          ))}
        {tab === "Services" && (
          <div className="demo-services">
            {[
              ["Storefront", "Node.js · port 3000"],
              ["API", "Node.js · port 8080"],
              ["PostgreSQL", "Isolated database · synthetic seed"],
              ["Redis", "Private cache"],
              ["RabbitMQ", "Private message broker"],
              ["Notifications", "Background worker"],
            ].map(([name, detail]) => (
              <div key={name}>
                <Box size={20} />
                <b>{name}</b>
                <small>{detail}</small>
              </div>
            ))}
          </div>
        )}
        {tab === "Logs" && (
          <pre className="demo-logs">
            <code>{`[simulation] namespace: heimdall-pr184-shopflow\n${stages
              .slice(0, progress)
              .map((s) => `[complete] ${s.name.toLowerCase()}\n`)
              .join(
                "",
              )}${progress < 5 ? `[running] ${stages[progress].name.toLowerCase()}` : "[ready] all smoke checks passed\n[info] each preview has its own data and workloads"}`}</code>
          </pre>
        )}
      </div>
      <div className="demo-footer">
        <small>No cluster connection required.</small>
        <button
          className="button button-light button-small"
          disabled={progress < 5}
          onClick={() => {
            setProgress(0);
            setTab("Pipeline");
          }}
        >
          <RotateCcw size={15} />
          {progress < 5 ? "Running walkthrough…" : "Replay deployment"}
        </button>
      </div>
    </Modal>
  );
}

function Docs({ onClose }: { onClose: () => void }) {
  return (
    <Modal title="Start with the foundations." onClose={onClose}>
      <p className="modal-intro">
        The CLI and renderer are available today. Explore the actual project
        documentation below.
      </p>
      <div className="doc-links">
        {[
          {
            title: "Configuration reference",
            text: "Services, workers, dependencies, resource sizes and trust rules.",
            url: "config-reference.md",
            icon: FileCode2,
          },
          {
            title: "Rendering guide",
            text: "The five stages, generated objects and secure pod defaults.",
            url: "rendering.md",
            icon: Layers3,
          },
          {
            title: "Project roadmap",
            text: "Completed milestones and the path to automated PR previews.",
            url: "phases.md",
            icon: GitBranch,
          },
        ].map((d) => (
          <a
            key={d.title}
            href={`/docs/${d.url}`}
            target="_blank"
            rel="noreferrer"
          >
            <d.icon size={22} />
            <div>
              <b>{d.title}</b>
              <p>{d.text}</p>
              <small>Read Markdown documentation</small>
            </div>
            <ArrowUpRight size={18} />
          </a>
        ))}
      </div>
      <div className="note">
        <Terminal size={18} />
        <p>
          Build from the repository with{" "}
          <code>go build -o bin/heimdall ./cmd/heimdall</code>. Requires Go 1.26
          or newer.
        </p>
      </div>
    </Modal>
  );
}

function HighlightedCode({ text }: { text: string }) {
  return (
    <>
      {text.split("\n").map((line, i) => (
        <span
          className={`code-line ${line.trimStart().startsWith("#") ? "code-comment" : ""}`}
          key={i}
        >
          <span className="line-number" aria-hidden="true">
            {i + 1}
          </span>
          <span>
            {line.trimStart().startsWith("#")
              ? line
              : line
                  .split(
                    /(\b[\w]+:|"[^"]*"|\b(?:true|private|48h|2Gi|3000|8080)\b)/g,
                  )
                  .map((part, j) => (
                    <span
                      key={j}
                      className={
                        part.endsWith(":")
                          ? "code-key"
                          : /^"|^(true|private|48h|2Gi|3000|8080)$/.test(part)
                            ? "code-value"
                            : ""
                      }
                    >
                      {part}
                    </span>
                  ))}
          </span>
        </span>
      ))}
    </>
  );
}

export default function App() {
  const [menuOpen, setMenuOpen] = useState(false);
  const [modal, setModal] = useState<"demo" | "docs" | null>(null);
  const [codeTab, setCodeTab] = useState<"config" | "commands">("config");
  const [copyMessage, setCopyMessage] = useState("");
  const copyTimer = useRef<number | undefined>(undefined);
  useEffect(() => {
    const elements = document.querySelectorAll(".reveal");
    const observer = new IntersectionObserver(
      (entries) =>
        entries.forEach((entry) => {
          if (entry.isIntersecting) {
            entry.target.classList.add("visible");
            observer.unobserve(entry.target);
          }
        }),
      { threshold: 0.1 },
    );
    elements.forEach((el) => observer.observe(el));
    return () => {
      observer.disconnect();
      window.clearTimeout(copyTimer.current);
    };
  }, []);
  async function copyCode() {
    try {
      await navigator.clipboard.writeText(
        codeTab === "config" ? config : commands,
      );
      setCopyMessage("Copied");
    } catch {
      setCopyMessage("Select the code to copy");
    }
    window.clearTimeout(copyTimer.current);
    copyTimer.current = window.setTimeout(() => setCopyMessage(""), 3000);
  }
  return (
    <>
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <div className="hero-wrap" id="top">
        <div className="hero-image" aria-hidden="true" />
        <div className="hero-shade" aria-hidden="true" />
        <div className="grain" aria-hidden="true" />
        <header className="header container">
          <Brand />
          <nav
            id="mobile-navigation"
            className={`nav ${menuOpen ? "is-open" : ""}`}
            aria-label="Main navigation"
          >
            <a href="#how-it-works" onClick={() => setMenuOpen(false)}>
              How it works
            </a>
            <a href="#features" onClick={() => setMenuOpen(false)}>
              Features
            </a>
            <a href="#security" onClick={() => setMenuOpen(false)}>
              Security
            </a>
            <button
              onClick={() => {
                setModal("docs");
                setMenuOpen(false);
              }}
            >
              Docs <ArrowUpRight size={13} />
            </button>
          </nav>
          <a className="button button-outline header-cta" href="#get-started">
            Get started <ArrowRight size={15} />
          </a>
          <button
            className="icon-button mobile-menu"
            aria-label={menuOpen ? "Close menu" : "Open menu"}
            aria-expanded={menuOpen}
            aria-controls="mobile-navigation"
            onClick={() => setMenuOpen(!menuOpen)}
          >
            {menuOpen ? <X /> : <Menu />}
          </button>
        </header>
        <main id="main">
          <section className="hero container" aria-labelledby="hero-title">
            <div className="hero-copy">
              <div className="badge entrance">
                <span className="status-dot" /> BUILT FOR DEVELOPERS. GUARDED BY
                DESIGN. <Sparkles size={12} />
              </div>
              <h1 id="hero-title" className="entrance delay-1">
                Every pull request.
                <br />A world
                <br />
                <span className="gradient-text">of its own.</span>
              </h1>
              <p className="hero-description entrance delay-2">
                Your frontend. Your API. Your database.
                <br className="desktop-break" /> One isolated environment to see
                the whole picture.
              </p>
              <p className="hero-subcopy entrance delay-2">
                Full-stack previews on Kubernetes, with security built in.
              </p>
              <div className="hero-actions entrance delay-3">
                <a className="button button-light" href="#get-started">
                  Explore Heimdall <ArrowRight size={17} />
                </a>
                <button
                  className="button button-glass"
                  onClick={() => setModal("demo")}
                >
                  <Play size={14} fill="currentColor" /> See how it works
                </button>
              </div>
              <div className="hero-footnote entrance delay-3">
                <Check size={13} /> Your infrastructure <span /> One
                configuration <span /> Secure defaults
              </div>
            </div>
            <div className="hero-cards entrance delay-4">
              <div className="preview-card glass">
                <div className="preview-card-heading">
                  <div className="square-icon">
                    <GitPullRequest size={24} />
                  </div>
                  <div>
                    <span className="card-kicker">
                      A PREVIEW OF WHAT’S POSSIBLE
                    </span>
                    <h2>From PR to possibility.</h2>
                  </div>
                  <span className="live-dot" />
                </div>
                <div className="pr-summary">
                  <span>
                    <GitBranch size={14} /> shopflow / feature-checkout
                  </span>
                  <span className="pr-number">#184</span>
                </div>
                <div className="pipeline-label">
                  <span>Full-stack deployment</span>
                  <span className="green-text">
                    <CheckCheck size={14} /> All systems ready
                  </span>
                </div>
                <div className="progress-track">
                  <span />
                </div>
                <div className="mini-pipeline">
                  {stages.map((s) => (
                    <div key={s.name}>
                      <span className="mini-stage">
                        <Check size={12} />
                      </span>
                      <span>{s.name}</span>
                      <span className="stage-order">{s.time}</span>
                    </div>
                  ))}
                </div>
                <div className="card-stats">
                  <div>
                    <b>1</b>
                    <span>ISOLATED NAMESPACE</span>
                  </div>
                  <div>
                    <b>5</b>
                    <span>DEPLOYMENT STAGES</span>
                  </div>
                  <div>
                    <b>3</b>
                    <span>NATIVE DEPENDENCIES</span>
                  </div>
                </div>
                <div className="card-bottom">
                  <span className="tiny-pill">
                    <LockKeyhole size={11} /> PRIVATE BY DEFAULT
                  </span>
                  <span className="illustration-label">
                    Illustrative preview
                  </span>
                </div>
              </div>
              <div className="ecosystem-card glass">
                <div className="ecosystem-heading">
                  <span>YOUR STACK. ALL TOGETHER.</span>
                  <Blocks size={14} />
                </div>
                <div className="marquee-mask">
                  <div className="marquee-track">
                    {[0, 1].map((n) => (
                      <div
                        className="marquee-group"
                        key={n}
                        aria-hidden={n === 1}
                      >
                        {technologies.map((t) => (
                          <span key={t.name}>
                            <t.icon size={20} />
                            {t.name}
                          </span>
                        ))}
                      </div>
                    ))}
                  </div>
                </div>
              </div>
            </div>
            <div className="hero-bottom">
              <a href="#how-it-works">
                <span className="scroll-icon">
                  <ArrowDown size={14} />
                </span>{" "}
                A better way to review what you build
              </a>
              <span>LESS SETUP. MORE SHIP.</span>
            </div>
          </section>
          <div className="content-wrap">
            <section
              className="section container"
              id="how-it-works"
              aria-labelledby="workflow-title"
            >
              <div className="section-heading reveal">
                <span className="eyebrow">01 / THE WORKFLOW</span>
                <div className="heading-row">
                  <h2 id="workflow-title">
                    Big changes.
                    <br />
                    <span className="muted">Small feedback loops.</span>
                  </h2>
                  <p>
                    Stop sharing a staging environment.
                    <br />
                    Give every change room to prove itself.
                  </p>
                </div>
              </div>
              <div className="workflow-grid">
                <article className="workflow-card reveal">
                  <div className="step-top">
                    <span>01</span>
                    <FileCode2 size={22} />
                  </div>
                  <h3>Describe your stack.</h3>
                  <p>
                    Define services, workers, dependencies and health checks in
                    one <code>heimdall.yaml</code>.
                  </p>
                  <div className="file-chip">
                    <FileCode2 size={15} />
                    <span>heimdall.yaml</span>
                    <Check size={14} />
                  </div>
                </article>
                <article className="workflow-card reveal">
                  <div className="step-top">
                    <span>02</span>
                    <ShieldCheck size={22} />
                  </div>
                  <h3>Validate. Then render.</h3>
                  <p>
                    Catch config errors and policy violations before generating
                    an ordered Kubernetes deployment plan.
                  </p>
                  <div className="terminal-line">
                    <span>$</span> heimdall validate{" "}
                    <span className="green-text">✓</span>
                  </div>
                </article>
                <article className="workflow-card reveal">
                  <div className="step-top">
                    <span>03</span>
                    <Layers3 size={22} />
                  </div>
                  <h3>Preview the whole picture.</h3>
                  <p>
                    Apply the staged manifests to your cluster. Test the UI, API
                    and data together in an isolated namespace.
                  </p>
                  <div className="stack-chips">
                    <span>web</span>
                    <span>api</span>
                    <span>database</span>
                    <span>worker</span>
                  </div>
                </article>
              </div>
              <div className="workflow-note reveal">
                <GitPullRequest size={16} />
                <span>
                  Coming next: automatic environments when PRs open, and cleanup
                  when they close.
                </span>
                <a href="#roadmap">
                  See the roadmap <ArrowRight size={13} />
                </a>
              </div>
            </section>
            <section
              className="section container features-section"
              id="features"
              aria-labelledby="features-title"
            >
              <div className="section-heading reveal">
                <span className="eyebrow">02 / BUILT FOR THE WHOLE STACK</span>
                <div className="heading-row">
                  <h2 id="features-title">
                    More than a frontend.
                    <br />
                    <span className="muted">A real environment.</span>
                  </h2>
                  <p>
                    Because the bugs that matter
                    <br />
                    live between your services.
                  </p>
                </div>
              </div>
              <div className="feature-grid">
                <article className="feature-card feature-wide glass reveal">
                  <div className="feature-text">
                    <div className="feature-icon">
                      <Blocks />
                    </div>
                    <h3>
                      All the moving parts.
                      <br />
                      One place to test.
                    </h3>
                    <p>
                      Frontend, API, background workers, PostgreSQL, Redis and
                      RabbitMQ. Declare their dependencies; Heimdall renders
                      them in the right order.
                    </p>
                    <span className="feature-tag">FULL-STACK BY DESIGN</span>
                  </div>
                  <div
                    className="stack-diagram"
                    aria-label="Web depends on API; API depends on PostgreSQL, Redis and RabbitMQ"
                  >
                    <div className="diagram-node">
                      <Code2 size={17} /> frontend <span>3000</span>
                    </div>
                    <div className="connector" />
                    <div className="diagram-node diagram-api">
                      <Box size={17} /> api <span>8080</span>
                    </div>
                    <div className="branch-connectors" />
                    <div className="diagram-dependencies">
                      <div>
                        <Database size={19} />
                        <span>Postgres</span>
                      </div>
                      <div>
                        <Layers3 size={19} />
                        <span>Redis</span>
                      </div>
                      <div>
                        <Workflow size={19} />
                        <span>RabbitMQ</span>
                      </div>
                    </div>
                    <span className="diagram-caption">
                      <span className="status-dot" /> one isolated namespace
                    </span>
                  </div>
                </article>
                <article className="feature-card glass reveal">
                  <div className="feature-icon">
                    <Database />
                  </div>
                  <h3>
                    Fresh data.
                    <br />
                    Clearer feedback.
                  </h3>
                  <p>
                    Run migrations, load synthetic seed data and clone a
                    baseline database. Each environment starts with a
                    predictable foundation.
                  </p>
                  <div className="data-flow">
                    <span>Migrate</span>
                    <ArrowRight size={13} />
                    <span>Seed</span>
                    <ArrowRight size={13} />
                    <span>Clone</span>
                  </div>
                  <span className="feature-tag">REPRODUCIBLE DATABASES</span>
                </article>
                <article className="feature-card glass reveal">
                  <div className="feature-icon">
                    <Terminal />
                  </div>
                  <h3>
                    Catch it before
                    <br />
                    the cluster does.
                  </h3>
                  <p>
                    Strict schema validation, source-located errors and
                    actionable hints. Find typos, dependency cycles and policy
                    violations early.
                  </p>
                  <div className="diagnostic">
                    <span>error[dependsOn.cycle]</span>
                    <code>api → worker → api</code>
                    <small>hint: Break the loop by removing an edge.</small>
                  </div>
                </article>
                <article className="feature-card glass reveal">
                  <div className="feature-icon">
                    <Workflow />
                  </div>
                  <h3>
                    Order matters.
                    <br />
                    So we make it explicit.
                  </h3>
                  <p>
                    Guardrails first. Dependencies next. Then data, application
                    waves and smoke tests. Every step is inspectable.
                  </p>
                  <div className="stage-bars">
                    {[28, 47, 65, 83, 100].map((width, i) => (
                      <span key={width} style={{ width: `${width}%` }}>
                        <i />
                        STAGE 0{i + 1}
                        <Check size={10} />
                      </span>
                    ))}
                  </div>
                </article>
                <article className="feature-card glass reveal">
                  <div className="feature-icon">
                    <FileCode2 />
                  </div>
                  <h3>
                    Your workflow.
                    <br />
                    Your infrastructure.
                  </h3>
                  <p>
                    A Go CLI, typed Kubernetes objects and JSON Schema for
                    editor completion. Start locally on kind; keep control of
                    your cluster.
                  </p>
                  <div className="platform-chips">
                    <span>Go</span>
                    <span>Kubernetes</span>
                    <span>Gateway API</span>
                  </div>
                  <button
                    className="text-link"
                    onClick={() => setModal("docs")}
                  >
                    Explore the documentation <ArrowUpRight size={14} />
                  </button>
                </article>
              </div>
            </section>
            <section
              className="security-section container reveal"
              id="security"
              aria-labelledby="security-title"
            >
              <div className="security-orbit" aria-hidden="true">
                <div className="orbit-ring ring-one" />
                <div className="orbit-ring ring-two" />
                <div className="orbit-ring ring-three" />
                <div className="shield-center">
                  <ShieldCheck size={48} strokeWidth={1.2} />
                </div>
                <span className="orbit-label label-one">
                  <LockKeyhole size={12} /> Restricted pods
                </span>
                <span className="orbit-label label-two">
                  <Network size={12} /> Isolated network
                </span>
                <span className="orbit-label label-three">
                  <Check size={12} /> Policy enforced
                </span>
              </div>
              <div className="security-copy">
                <span className="eyebrow">03 / THE GUARDIAN OF YOUR STACK</span>
                <h2 id="security-title">
                  Move fast.
                  <br />
                  <span className="gradient-text">Keep your guard up.</span>
                </h2>
                <p>
                  Pull requests are untrusted input. Heimdall’s renderer puts
                  secure defaults into every pod, and checks configuration
                  against policy before it reaches your cluster.
                </p>
                <ul className="security-list">
                  <li>
                    <Check size={15} /> Non-root containers. No privilege
                    escalation.
                  </li>
                  <li>
                    <Check size={15} /> Default-deny networking and resource
                    quotas.
                  </li>
                  <li>
                    <Check size={15} /> No mounted service-account tokens.
                  </li>
                  <li>
                    <Check size={15} /> PRs can narrow the baseline policy,
                    never loosen it.
                  </li>
                </ul>
                <button className="text-link" onClick={() => setModal("docs")}>
                  Read the security & rendering guide <ArrowUpRight size={15} />
                </button>
              </div>
            </section>
            <section
              className="section container setup-section"
              id="get-started"
              aria-labelledby="setup-title"
            >
              <div className="setup-copy reveal">
                <span className="eyebrow">
                  04 / LESS CEREMONY. MORE CLARITY.
                </span>
                <h2 id="setup-title">
                  One file.
                  <br />
                  <span className="muted">Your entire stack.</span>
                </h2>
                <p>
                  Start with the working CLI. Validate the included ShopFlow
                  example, inspect the plan, and export the manifests.
                </p>
                <div className="setup-list">
                  <div>
                    <span>01</span>
                    <div>
                      <b>Build the CLI</b>
                      <p>Go 1.26+ and the Heimdall repository.</p>
                    </div>
                  </div>
                  <div>
                    <span>02</span>
                    <div>
                      <b>Define your environment</b>
                      <p>Services, data and policies in YAML.</p>
                    </div>
                  </div>
                  <div>
                    <span>03</span>
                    <div>
                      <b>Inspect what gets deployed</b>
                      <p>Deterministic, stage-by-stage manifests.</p>
                    </div>
                  </div>
                </div>
                <button
                  className="button button-outline"
                  onClick={() => {
                    setCodeTab("commands");
                    setCopyMessage("");
                  }}
                >
                  Show quick-start commands <Terminal size={15} />
                </button>
                <p className="setup-caption">
                  Placeholder images are for inspecting the plan.
                  <br />
                  Use digest-pinned application images for deployment.
                </p>
              </div>
              <div className="code-window glass reveal">
                <div className="code-window-top">
                  <div className="window-dots">
                    <i />
                    <i />
                    <i />
                  </div>
                  <span>YOUR STACK, AS CODE</span>
                  <LockKeyhole size={13} />
                </div>
                <div
                  className="code-tabs"
                  role="tablist"
                  aria-label="Getting started examples"
                >
                  <button
                    role="tab"
                    id="config-tab"
                    aria-selected={codeTab === "config"}
                    aria-controls="config-panel"
                    onClick={() => {
                      setCodeTab("config");
                      setCopyMessage("");
                    }}
                    onKeyDown={(e) => {
                      if (e.key === "ArrowRight") {
                        setCodeTab("commands");
                        document.getElementById("commands-tab")?.focus();
                      }
                    }}
                  >
                    <FileCode2 size={14} /> heimdall.yaml
                  </button>
                  <button
                    role="tab"
                    id="commands-tab"
                    aria-selected={codeTab === "commands"}
                    aria-controls="commands-panel"
                    onClick={() => {
                      setCodeTab("commands");
                      setCopyMessage("");
                    }}
                    onKeyDown={(e) => {
                      if (e.key === "ArrowLeft") {
                        setCodeTab("config");
                        document.getElementById("config-tab")?.focus();
                      }
                    }}
                  >
                    <Terminal size={14} /> Quick start
                  </button>
                  <button
                    className="copy-button"
                    onClick={copyCode}
                    aria-label="Copy code"
                  >
                    {copyMessage === "Copied" ? (
                      <Check size={15} />
                    ) : (
                      <Copy size={15} />
                    )}
                  </button>
                </div>
                <pre
                  role="tabpanel"
                  id={`${codeTab}-panel`}
                  aria-labelledby={`${codeTab}-tab`}
                  tabIndex={0}
                >
                  <code>
                    <HighlightedCode
                      text={codeTab === "config" ? config : commands}
                    />
                  </code>
                </pre>
                <div className="code-window-bottom">
                  <span>
                    <span className="status-dot" />{" "}
                    {codeTab === "config"
                      ? "Schema v1 · editor-friendly"
                      : "Run from the repository root"}
                  </span>
                  <span role="status" aria-live="polite">
                    {copyMessage || (codeTab === "config" ? "YAML" : "SHELL")}
                  </span>
                </div>
              </div>
            </section>
            <section
              className="section container roadmap-section"
              id="roadmap"
              aria-labelledby="roadmap-title"
            >
              <div className="section-heading reveal">
                <span className="eyebrow">05 / BUILDING IN THE OPEN</span>
                <div className="heading-row">
                  <h2 id="roadmap-title">
                    A solid foundation.
                    <br />
                    <span className="muted">An ambitious horizon.</span>
                  </h2>
                  <p>
                    Real progress, clearly marked.
                    <br />
                    Here’s where Heimdall stands today.
                  </p>
                </div>
              </div>
              <div className="roadmap-grid">
                <article className="roadmap-card reveal">
                  <span className="status-pill">
                    <Check size={13} /> AVAILABLE TODAY
                  </span>
                  <h3>The foundation is here.</h3>
                  <p>
                    The core config and rendering layers are complete and
                    runnable.
                  </p>
                  <ul>
                    <li>
                      <Check size={14} /> Strict config validation & JSON Schema
                    </li>
                    <li>
                      <Check size={14} /> Baseline comparison & trust policy
                    </li>
                    <li>
                      <Check size={14} /> Secure, staged Kubernetes rendering
                    </li>
                    <li>
                      <Check size={14} /> ShopFlow demo & kind end-to-end tests
                    </li>
                  </ul>
                  <button
                    className="text-link"
                    onClick={() => {
                      setCodeTab("commands");
                      document
                        .getElementById("get-started")
                        ?.scrollIntoView({ behavior: "smooth" });
                    }}
                  >
                    Try the CLI <ArrowRight size={15} />
                  </button>
                </article>
                <article className="roadmap-card roadmap-planned reveal">
                  <span className="tiny-pill">
                    <GitBranch size={12} /> ON THE ROADMAP
                  </span>
                  <h3>The preview lifecycle.</h3>
                  <p>
                    The next phases connect these foundations into the full
                    product.
                  </p>
                  <ul>
                    <li>
                      <span className="planned-dot" /> Local engine &
                      environment controller
                    </li>
                    <li>
                      <span className="planned-dot" /> GitHub PR automation &
                      diagnostics
                    </li>
                    <li>
                      <span className="planned-dot" /> Authenticated previews &
                      lifecycle cleanup
                    </li>
                    <li>
                      <span className="planned-dot" /> AWS validation &
                      environment dashboard
                    </li>
                  </ul>
                  <button
                    className="text-link"
                    onClick={() => setModal("docs")}
                  >
                    Explore the full roadmap <ArrowUpRight size={15} />
                  </button>
                </article>
              </div>
            </section>
            <section
              className="section container faq-section"
              aria-labelledby="faq-title"
            >
              <div className="reveal">
                <span className="eyebrow">A LITTLE MORE CONTEXT</span>
                <h2 id="faq-title">
                  Good questions.
                  <br />
                  <span className="muted">Straight answers.</span>
                </h2>
              </div>
              <div className="faq-list reveal">
                {[
                  [
                    "What can I use today?",
                    "The CLI validates heimdall.yaml, compares PR configuration against a baseline, exports JSON Schema and renders staged Kubernetes manifests. The full automatic PR lifecycle is on the roadmap.",
                  ],
                  [
                    "Do I need an AWS account?",
                    "No. Start with the CLI without cluster access. The included end-to-end example uses kind locally. EKS validation and the customer-owned cluster integration are planned phases.",
                  ],
                  [
                    "Which parts of my stack are supported?",
                    "The configuration supports frontend and API services, background workers, PostgreSQL, Redis and RabbitMQ, plus migrations, synthetic seed data and smoke tests. Application images are pinned by digest when rendering a deployable plan.",
                  ],
                  [
                    "How is my preview isolated?",
                    "The renderer creates a dedicated namespace, restricted pod security, resource quotas and default-deny network policies with explicit allows. A namespace alone is not a hard security boundary for hostile multi-tenant workloads; the roadmap adds customer-owned clusters and further isolation checks.",
                  ],
                  [
                    "Can I connect production data?",
                    "Use synthetic seed data or manually approved sanitised fixtures. Heimdall’s design keeps production credentials out of preview workloads. Automated production snapshots are not part of the current implementation.",
                  ],
                ].map(([q, a]) => (
                  <details key={q}>
                    <summary>
                      {q}
                      <ChevronDown size={17} />
                    </summary>
                    <p>{a}</p>
                  </details>
                ))}
              </div>
            </section>
            <section className="final-cta container reveal">
              <div className="cta-glow" aria-hidden="true" />
              <span className="eyebrow">
                <GitPullRequest size={14} /> BUILT FOR YOUR NEXT CHANGE
              </span>
              <h2>
                Give your code
                <br />
                <span className="gradient-text">a world of its own.</span>
              </h2>
              <p>Less staging friction. More confidence in every review.</p>
              <div className="hero-actions">
                <a href="#get-started" className="button button-light">
                  Start with Heimdall <ArrowRight size={16} />
                </a>
                <button
                  className="button button-glass"
                  onClick={() => setModal("docs")}
                >
                  <BookOpen size={15} /> Read the docs
                </button>
              </div>
            </section>
          </div>
        </main>
        <footer className="footer container">
          <div>
            <Brand footer />
            <p>The guardian of your next deployment.</p>
          </div>
          <div className="footer-links">
            <a href="#how-it-works">Workflow</a>
            <a href="#security">Security</a>
            <a href="#roadmap">Roadmap</a>
            <button onClick={() => setModal("docs")}>
              Documentation <ArrowUpRight size={12} />
            </button>
          </div>
          <div className="footer-bottom">
            <span>Heimdall · Built with intention.</span>
            <span>
              <span className="status-dot" /> Config & renderer available
            </span>
            <a href="#top">
              Back to top <ArrowUpRight size={13} />
            </a>
          </div>
        </footer>
      </div>
      {modal === "demo" && <Demo onClose={() => setModal(null)} />}
      {modal === "docs" && <Docs onClose={() => setModal(null)} />}
    </>
  );
}
