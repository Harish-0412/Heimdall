# Heimdall web experience

React + TypeScript landing page and P11 preview dashboard. Existing locally
bundled fonts, logo and landing imagery are retained. The dashboard uses a warm
canvas, dark navigation rail, environment runway and contextual inspector.

## Run locally

Requires Node.js 20.19+ or 22.12+ and npm. From this directory:

```powershell
npm install
npm run docs:sync
npm run dev
```

Open `http://127.0.0.1:5173/` for the product introduction or
`http://127.0.0.1:5173/dashboard` for the flight deck. Deep links include
`?view=setup`, `?view=connections`, `?view=docs`, `?view=demo` and `?env=<id>`.

## Dashboard behavior

- The default workspace is explicitly sample data. Actions alter only browser
  state, and accepted retry/reset/delete operations remain pending reconciliation.
- Search, phase filters, five-stage status, diagnosis evidence, declared services,
  timeline, redacted log tails and action confirmations work on desktop/mobile.
- Connect from Connections or the sidebar using an operator-issued tenant user
  token. Tokens stay in memory; disconnect/reload clears the session. No browser
  login/session service or role discovery is implemented; the API enforces roles.
- The development server proxies `/v1` to `http://127.0.0.1:8080`. Start the real
  API using [control-plane setup](../docs/control-plane.md). Live requests use
  the existing public contract and versioned, idempotent actions.
- Lists/timelines follow complete bounded pagination. Visible live workspaces
  refresh every 15 seconds; outages preserve the last successful snapshot.
  Logs are requested on demand, scoped to the generation and discarded on
  selection/generation changes or after two minutes.
- The live API currently omits branch/current-build metadata and allocation cost.
  Missing data is labelled unavailable. The sample estimate documents its rates
  and excludes storage/shared infrastructure; it is not a bill.
- Setup checkmarks are manual browser-local progress, separate from observed
  cluster heartbeats. The guide links to actual App, Helm and CLI procedures.
- The document reader renders safe headings, paragraphs, basic lists, tables,
  inline links/code and fenced code. HTML remains text. Mermaid and nested
  Markdown structures are shown as source; unpublished repository links are
  displayed without a clickable target.

## Build and host

```powershell
npm run build
npm run preview
```

The build refreshes `public/docs` from repository documentation and copies four
public contract/setup references. Deploy `dist/` to a static host with an
`index.html` fallback for `/dashboard` deep links. Assets are root-relative.
For live use, provide an HTTPS same-origin reverse proxy for `/v1`; the Vite
development proxy is not part of the production bundle. Production sign-in,
P6 live acceptance and P7–P10 launch validation remain separate work.

## Verification and launch rehearsal

```powershell
npx playwright install chromium
npm run test:e2e
# With the development server already running:
npm run demo:record
```

Browser tests cover existing landing behavior and dashboard sample/live flows,
pagination, authentication, permission errors, action conflicts, log polling,
manual onboarding, documentation errors, six launch scenes, keyboard dialogs and
desktop/mobile overflow. The recording command saves sample screenshots and a
WebM backup to `../out/p11`. It blocks API calls and labels the recording as a
browser simulation, not live launch evidence. Set `HEIMDALL_DEMO_ORIGIN` to a
different running frontend origin if needed.

The complete [design and implementation plan](../docs/dashboard-design.md),
[quickstart](../docs/dashboard-quickstart.md) and
[launch runbook](../docs/launch-demo.md) define remaining acceptance criteria.
P11 is started; it is complete only after the real demo/recording and an
independent user reaches a working preview from the docs in under 30 minutes.
