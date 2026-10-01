# Heimdall landing page

A responsive React + TypeScript landing page based on the supplied Glassmorphism Trust Hero template. Uses the original hero background and `Heimdall_2.jpg` logo, both served locally.

## Development

Requires Node.js 20.19+ or 22.12+ and npm.

```powershell
cd C:\SideQuest\CLD\heimdall\web
npm install
npm run dev
```

## Production

```powershell
npm run build
npm run preview
```

Deploy `dist/` to any static host. The app currently uses root-relative asset paths; configure Vite's base and asset paths together if hosting under a subdirectory.

## Browser checks

```powershell
npx playwright install chromium
npm run test:e2e
```

Tests cover desktop/mobile overflow, loaded assets, walkthrough tabs and replay, modal focus restoration, clipboard contents, docs, FAQs, mobile navigation and reduced motion.

## Content and behavior

- CLI and secure rendering capabilities reflect the repository's completed P0/P0.1/P1 phases. Automated PR lifecycle features are clearly marked as planned.
- The preview walkthrough is a browser-only simulation, with replay, pipeline, services and logs views.
- Documentation links open local copies of the repository's Markdown guides in `public/docs`. Refresh those files when the upstream documentation changes.
- The setup example has configuration and command tabs, with clipboard feedback.
- Includes mobile navigation, keyboard-accessible dialogs, native FAQ disclosures, visible focus styles, skip link, scroll reveals, hover effects, a technology marquee and reduced-motion support.
- Variable fonts and images are bundled locally; the page makes no third-party runtime requests.

Template reference: https://21st.dev/@jahed/components/glassmorphism-trust-hero
