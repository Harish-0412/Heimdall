import { useEffect, useState } from "react";
import {
  ArrowRight,
  ArrowUpRight,
  BookOpen,
  Check,
  CheckCircle2,
  Copy,
  FileCode2,
  GitBranch,
  Network,
  Rocket,
  Search,
  ShieldCheck,
  Terminal,
} from "lucide-react";

const guides = [
  {
    file: "dashboard-quickstart.md",
    title: "Your first preview",
    tag: "START HERE",
    description: "From a local dashboard to a real, verified preview.",
    icon: Rocket,
  },
  {
    file: "control-plane.md",
    title: "Connect your cluster",
    tag: "OPERATIONS",
    description: "Provision the control plane and enroll an outbound agent.",
    icon: Network,
  },
  {
    file: "github-app-setup.md",
    title: "GitHub App & CI",
    tag: "INTEGRATION",
    description: "Repository installation, trusted builds and acceptance.",
    icon: GitBranch,
  },
  {
    file: "diagnostics.md",
    title: "Find the root cause",
    tag: "TROUBLESHOOTING",
    description: "Diagnosis codes, evidence and a path to recovery.",
    icon: ShieldCheck,
  },
  {
    file: "dashboard-design.md",
    title: "The dashboard blueprint",
    tag: "DESIGN",
    description: "Every component, state and implementation milestone.",
    icon: FileCode2,
  },
  {
    file: "launch-demo.md",
    title: "Launch demonstration",
    tag: "SHOW & TELL",
    description: "A repeatable story with a clear live acceptance checklist.",
    icon: BookOpen,
  },
];

export function CopyCommand({ code }: { code: string }) {
  const [message, setMessage] = useState("");
  async function copy() {
    try {
      await navigator.clipboard.writeText(code);
      setMessage("Copied");
    } catch {
      setMessage("Select the command to copy");
    }
  }
  return (
    <div className="fd-command">
      <pre tabIndex={0}>
        <code>{code}</code>
      </pre>
      <button onClick={copy} aria-label="Copy command">
        <Copy size={15} />
      </button>
      <span role="status">{message}</span>
    </div>
  );
}

const setupSteps = [
  {
    title: "Install your GitHub App",
    time: "~5 min",
    icon: GitBranch,
    body: "Install your own Heimdall App on the selected repository. An operator registers its installation, repository and reviewed workflow with your tenant.",
    link: "github-app-setup.md",
    label: "App installation guide",
    command: null,
  },
  {
    title: "Bring your cluster online",
    time: "~10 min",
    icon: Network,
    body: "Provision a customer-owned cluster, create a one-use enrollment token, and install the Helm agent using a reviewed image digest and your platform settings.",
    link: "control-plane.md",
    label: "Enrollment & Helm guide",
    command: null,
  },
  {
    title: "Define your preview",
    time: "~5 min",
    icon: FileCode2,
    body: "Generate the starter files locally, adapt the configuration to your Dockerfile and app port, then review and commit them to the default branch. Replace the workflow placeholder before running.",
    link: "github-app-setup.md",
    label: "Configure the trusted workflow",
    command:
      "heimdall init --workflow 'OWNER/REPO/.github/workflows/heimdall-preview.yml@<reviewed-40-character-sha>' --out-dir ./preview-setup --port 3000",
  },
  {
    title: "Open a PR. Verify the preview.",
    time: "~5 min",
    icon: Rocket,
    body: "Open a same-repository pull request. Verify the build callback, agent heartbeat, all five stages and the protected preview URL. Close the PR and verify namespace cleanup.",
    link: "dashboard-quickstart.md",
    label: "First-preview acceptance checklist",
    command: null,
  },
];

function loadChecks(): number[] {
  try {
    const value: unknown = JSON.parse(
      localStorage.getItem("heimdall-setup-checks") || "[]",
    );
    return Array.isArray(value)
      ? value.filter((v): v is number => Number.isInteger(v) && v >= 0 && v < 4)
      : [];
  } catch {
    return [];
  }
}

export function Onboarding() {
  const [checked, setChecked] = useState<number[]>(loadChecks);
  const [active, setActive] = useState(0);
  function toggle(index: number) {
    const next = checked.includes(index)
      ? checked.filter((v) => v !== index)
      : [...checked, index];
    setChecked(next);
    try {
      localStorage.setItem("heimdall-setup-checks", JSON.stringify(next));
    } catch {
      /* Optional local progress. */
    }
  }
  const step = setupSteps[active];
  return (
    <div className="fd-guide-layout">
      <section className="fd-setup-intro">
        <span className="fd-kicker">A GOOD START CHANGES EVERYTHING</span>
        <h2>
          One PR away
          <br />
          from your first preview.
        </h2>
        <p>
          Four steps. Your infrastructure. A clear path from setup to something
          your team can review.
        </p>
        <div className="fd-setup-progress">
          <div>
            <span style={{ width: `${checked.length * 25}%` }} />
          </div>
          <b>{checked.length} of 4 checked off</b>
        </div>
        <p className="fd-small">
          Your checklist is saved in this browser. Steps are self-reported;
          connection status is verified separately.
        </p>
        <a
          className="fd-text-link"
          href="/docs/dashboard-quickstart.md"
          target="_blank"
          rel="noreferrer"
        >
          Read the complete quickstart <ArrowUpRight size={15} />
        </a>
      </section>
      <section className="fd-setup-steps" aria-label="Onboarding steps">
        {setupSteps.map((s, i) => (
          <div
            key={s.title}
            className={`fd-setup-step ${active === i ? "active" : ""}`}
          >
            <button
              className="fd-step-select"
              onClick={() => setActive(i)}
              aria-expanded={active === i}
            >
              <span
                className={`fd-step-number ${checked.includes(i) ? "done" : ""}`}
              >
                {checked.includes(i) ? <Check size={17} /> : `0${i + 1}`}
              </span>
              <span>
                <b>{s.title}</b>
                <small>{s.time}</small>
              </span>
              <ArrowRight size={18} />
            </button>
            {active === i && (
              <div className="fd-step-body">
                <p>{step.body}</p>
                {step.command && <CopyCommand code={step.command} />}
                <a
                  className="fd-text-link"
                  href={`/docs/${step.link}`}
                  target="_blank"
                  rel="noreferrer"
                >
                  {step.label} <ArrowUpRight size={14} />
                </a>
                <label className="fd-check-label">
                  <input
                    type="checkbox"
                    checked={checked.includes(i)}
                    onChange={() => toggle(i)}
                  />{" "}
                  I’ve completed and verified this step
                </label>
              </div>
            )}
          </div>
        ))}
        <div className="fd-setup-note">
          <ShieldCheck size={19} />
          <p>
            The 30-minute target assumes the operator has prepared the App,
            registry, control plane and cluster. Public launch also requires
            P7–P10 validation.
          </p>
        </div>
      </section>
    </div>
  );
}

const sourceReferences = new Set([
  "api/openapi.yaml",
  "internal/api/v1alpha1/previewenvironment_types.go",
  "web/README.md",
  "charts/heimdall-agent/README.md",
]);
function headingID(text: string) {
  return `guide-${text
    .toLowerCase()
    .replace(/[^a-z0-9\s-]/g, "")
    .trim()
    .replace(/\s+/g, "-")}`;
}
function inline(text: string, file: string) {
  return text
    .split(/(\[[^\]]+\]\([^\s)]+\)|`[^`]+`|\*\*[^*]+\*\*)/g)
    .map((part, i) => {
      const link = part.match(/^\[([^\]]+)\]\(([^\s)]+)\)$/);
      if (link) {
        if (link[2].startsWith("#"))
          return (
            <a key={i} href={`#guide-${link[2].slice(1)}`}>
              {link[1]}
            </a>
          );
        const resolved = new URL(
          link[2],
          `https://guides.invalid/docs/${file}`,
        );
        const local = resolved.origin === "https://guides.invalid";
        const reference = resolved.pathname.slice(1);
        const url = local
          ? resolved.pathname.startsWith("/docs/")
            ? resolved.pathname + resolved.hash
            : sourceReferences.has(reference)
              ? `/docs/reference/${reference}`
              : null
          : ["https:", "http:"].includes(resolved.protocol)
            ? resolved.href
            : null;
        return url ? (
          <a key={i} href={url} target="_blank" rel="noreferrer">
            {link[1]}
          </a>
        ) : (
          link[1]
        );
      }
      if (part.startsWith("`")) return <code key={i}>{part.slice(1, -1)}</code>;
      if (part.startsWith("**"))
        return <strong key={i}>{part.slice(2, -2)}</strong>;
      return part;
    });
}

function Markdown({ text, file }: { text: string; file: string }) {
  const blocks = text.replace(/\r/g, "").split(/(```[\s\S]*?```)/g);
  return (
    <div className="fd-markdown">
      {blocks.map((block, index) => {
        if (block.startsWith("```"))
          return (
            <pre key={index} tabIndex={0}>
              <code>
                {block.replace(/^```[^\n]*\n/, "").replace(/```$/, "")}
              </code>
            </pre>
          );
        return block
          .split(/\n\s*\n/)
          .filter(Boolean)
          .map((paragraph, i) => {
            const heading = paragraph.match(/^(#{1,6})\s+([^\n]+)$/);
            if (heading)
              return heading[1].length <= 2 ? (
                <h3 id={headingID(heading[2])} key={`${index}-${i}`}>
                  {inline(heading[2], file)}
                </h3>
              ) : (
                <h4 id={headingID(heading[2])} key={`${index}-${i}`}>
                  {inline(heading[2], file)}
                </h4>
              );
            if (paragraph.trim() === "---") return <hr key={`${index}-${i}`} />;
            const lines = paragraph.split("\n");
            if (
              lines.length > 1 &&
              /^\|/.test(lines[0]) &&
              /^\|[\s:|-]+\|$/.test(lines[1])
            ) {
              const cells = (row: string) =>
                row
                  .replace(/^\||\|$/g, "")
                  .split(/(?<!\\)\|/)
                  .map((cell) => cell.trim().replace(/\\\|/g, "|"));
              return (
                <table key={`${index}-${i}`}>
                  <thead>
                    <tr>
                      {cells(lines[0]).map((cell, col) => (
                        <th key={col}>{inline(cell, file)}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {lines
                      .slice(2)
                      .filter((line) => line.startsWith("|"))
                      .map((row, number) => (
                        <tr key={number}>
                          {cells(row).map((cell, col) => (
                            <td key={col}>{inline(cell, file)}</td>
                          ))}
                        </tr>
                      ))}
                  </tbody>
                </table>
              );
            }
            if (lines.every((line) => /^\s*[-*]\s/.test(line)))
              return (
                <ul key={`${index}-${i}`}>
                  {lines.map((line, n) => (
                    <li key={n}>
                      {inline(line.replace(/^\s*[-*]\s/, ""), file)}
                    </li>
                  ))}
                </ul>
              );
            if (lines.every((line) => /^\s*\d+\.\s/.test(line)))
              return (
                <ol key={`${index}-${i}`}>
                  {lines.map((line, n) => (
                    <li key={n}>
                      {inline(line.replace(/^\s*\d+\.\s/, ""), file)}
                    </li>
                  ))}
                </ol>
              );
            return <p key={`${index}-${i}`}>{inline(paragraph, file)}</p>;
          });
      })}
    </div>
  );
}

export function Library() {
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [content, setContent] = useState("");
  const [error, setError] = useState("");
  useEffect(() => {
    if (!selected) return;
    const controller = new AbortController();
    setContent("");
    setError("");
    fetch(`/docs/${selected}`, { signal: controller.signal })
      .then(async (response) => {
        if (!response.ok)
          throw new Error(
            "This guide could not be loaded. Try again or open the source document.",
          );
        const text = await response.text();
        if (!text.startsWith("# "))
          throw new Error("The guide is unavailable in this build.");
        setContent(text);
      })
      .catch((error) => {
        if (!controller.signal.aborted) setError(error.message);
      });
    return () => controller.abort();
  }, [selected]);
  const filtered = guides.filter((g) =>
    `${g.title} ${g.description} ${g.tag}`
      .toLowerCase()
      .includes(search.toLowerCase()),
  );
  return (
    <div className="fd-library">
      <div className="fd-library-top">
        <div>
          <span className="fd-kicker">THE ANSWER IS CLOSE BY</span>
          <h2>A little less guesswork.</h2>
          <p>
            Guides for the first preview, the next failure, and everything in
            between.
          </p>
        </div>
        <label className="fd-search">
          <Search size={17} />
          <input
            aria-label="Search documentation"
            placeholder="Find a guide…"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </label>
      </div>
      <div className="fd-guide-grid">
        {filtered.map((g) => (
          <button
            key={g.file}
            className={`fd-guide-card ${selected === g.file ? "selected" : ""}`}
            onClick={() => setSelected(g.file)}
          >
            <g.icon size={25} />
            <span className="fd-kicker">{g.tag}</span>
            <h3>{g.title}</h3>
            <p>{g.description}</p>
            <span className="fd-text-link">
              Read the guide <ArrowRight size={15} />
            </span>
          </button>
        ))}
      </div>
      {!filtered.length && (
        <div className="fd-empty">
          <Search />
          <h3>No guides match that search.</h3>
          <p>Try “cluster”, “preview” or “diagnosis”.</p>
        </div>
      )}
      {selected && (
        <section className="fd-reader" aria-label="Documentation reader">
          <div className="fd-section-head">
            <h3>{guides.find((g) => g.file === selected)?.title}</h3>
            <a
              href={`/docs/${selected}`}
              target="_blank"
              rel="noreferrer"
              className="fd-text-link"
            >
              Open source <ArrowUpRight size={14} />
            </a>
          </div>
          {error ? (
            <p role="alert">{error}</p>
          ) : content ? (
            <Markdown text={content} file={selected} />
          ) : (
            <p role="status">Loading guide…</p>
          )}
        </section>
      )}
      <section className="fd-diagnosis-index">
        <span className="fd-kicker">WHEN SOMETHING STOPS</span>
        <h3>Start with the diagnosis code.</h3>
        <div>
          {[
            [
              "MIGRATION_FAILED",
              "Check migration ordering and database permissions. Fix the migration and push a new commit.",
            ],
            [
              "IMAGE_PULL_FAILED",
              "Check the image digest, registry path and agent pull permissions.",
            ],
            [
              "trust.visibility",
              "A PR cannot loosen the default-branch visibility. Restore the baseline or narrow access.",
            ],
          ].map(([code, text]) => (
            <article key={code}>
              <code>{code}</code>
              <p>{text}</p>
            </article>
          ))}
        </div>
        <a
          className="fd-text-link"
          href="/docs/diagnostics.md"
          target="_blank"
          rel="noreferrer"
        >
          All diagnosis codes <ArrowUpRight size={14} />
        </a>
      </section>
    </div>
  );
}

const scenes = [
  {
    title: "Two PRs. Two worlds.",
    code: "01 / PARALLEL PREVIEWS",
    body: "ShopFlow #184 and #185 have separate environments. Their namespaces and databases are distinct.",
    evidence:
      "#184 → pr184-shopflow-x7d2\n#185 → pr185-shopflow-k9m4\nSeparate namespaces · separate live databases",
    action: "Next: data isolation",
  },
  {
    title: "Change one. Keep the other.",
    code: "02 / DATA ISOLATION",
    body: "In the live run, create an order in #184 and verify it never appears in #185. This browser scene illustrates the expected result.",
    evidence:
      "#184 orders: 1\n#185 orders: 0\nLive proof: query both APIs and save the results",
    action: "Next: diagnose a failure",
  },
  {
    title: "A failure with a way forward.",
    code: "03 / MIGRATION DIAGNOSIS",
    body: "A deliberately broken migration stops #185 at the database stage. The diagnosis identifies the failed step and suggests a correction.",
    evidence:
      "MIGRATION_FAILED\nbaseline-db/migrate\nFix the migration, then push a new generation.",
    action: "Next: reset the data",
  },
  {
    title: "Back to a clean baseline.",
    code: "04 / AUDITED RESET",
    body: "Reset suspends application workloads, recreates the live database from its baseline, clears dependency state and resumes services. A reset causes a maintenance window.",
    evidence:
      "Reset requested → application suspended\nBaseline cloned → dependencies cleared\nApplication resumed → smoke tests passed",
    action: "Next: refuse unsafe input",
  },
  {
    title: "Keep the guardrails in place.",
    code: "05 / TRUST CHECK",
    body: "A PR tries to change preview visibility from private to public. The default-branch trust comparison refuses the change before deployment.",
    evidence:
      "baseline: visibility = private\npull request: visibility = public\nREFUSED · trust.visibility",
    action: "Next: close and clean up",
  },
  {
    title: "Review finished. Resources gone.",
    code: "06 / VERIFIED CLEANUP",
    body: "Closing #185 changes desired state to Destroyed. The live run verifies workload, PVC and namespace deletion while #184 remains healthy.",
    evidence:
      "#185 → Destroying → Destroyed\nNamespace absent · PVCs absent\n#184 → Ready · unaffected",
    action: "Replay the story",
  },
];

export function LaunchDemo() {
  const [scene, setScene] = useState(0);
  const current = scenes[scene];
  return (
    <div className="fd-launch">
      <div className="fd-launch-heading">
        <span className="fd-mode-pill">Browser simulation</span>
        <h2>
          The whole story.
          <br />
          <span>In six small moments.</span>
        </h2>
        <p>
          A repeatable demonstration of the preview lifecycle. All values in
          this walkthrough are illustrative; the launch guide describes the
          required live evidence.
        </p>
      </div>
      <div className="fd-scenes" aria-label="Demo scenes">
        {scenes.map((s, i) => (
          <button
            key={s.code}
            onClick={() => setScene(i)}
            aria-current={scene === i ? "step" : undefined}
          >
            <span>{i < scene ? <CheckCircle2 size={18} /> : `0${i + 1}`}</span>
            <b>{s.code.split(" / ")[1]}</b>
          </button>
        ))}
      </div>
      <section className="fd-scene" aria-live="polite">
        <div>
          <span className="fd-kicker">{current.code}</span>
          <h3>{current.title}</h3>
          <p>{current.body}</p>
          <button
            className="fd-primary"
            onClick={() => setScene((scene + 1) % scenes.length)}
          >
            {current.action}
            <ArrowRight size={16} />
          </button>
        </div>
        <div className="fd-evidence">
          <Terminal size={20} />
          <span>ILLUSTRATIVE EVIDENCE</span>
          <pre>{current.evidence}</pre>
        </div>
      </section>
      <div className="fd-demo-footer">
        <p>
          <ShieldCheck size={17} /> Launch readiness requires the live run, a
          recorded backup and a timed first-user walkthrough.
        </p>
        <a
          className="fd-text-link"
          href="/docs/launch-demo.md"
          target="_blank"
          rel="noreferrer"
        >
          Open the presenter script <ArrowUpRight size={15} />
        </a>
      </div>
    </div>
  );
}
