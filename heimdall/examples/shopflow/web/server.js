// ShopFlow storefront. Zero dependencies: it serves one page and a runtime
// config script. The API's URL is read at request time from the environment
// Heimdall injects (HEIMDALL_PUBLIC_URL_API), so the same image works in every
// preview: nothing environment-specific is baked in at build time.
import http from 'node:http';
import { readFile } from 'node:fs/promises';

const port = Number(process.env.PORT ?? 3000);
const page = await readFile(new URL('./index.html', import.meta.url));

const runtimeConfig = () =>
  `window.SHOPFLOW = ${JSON.stringify({
    apiUrl: process.env.HEIMDALL_PUBLIC_URL_API ?? '',
    pr: process.env.HEIMDALL_PR ?? '',
    sha: (process.env.HEIMDALL_SHA ?? '').slice(0, 7),
  })};\n`;

const security = {
  'x-content-type-options': 'nosniff',
  'referrer-policy': 'no-referrer',
  'x-frame-options': 'DENY',
};

const server = http.createServer((req, res) => {
  const path = new URL(req.url, 'http://x').pathname;
  if (req.method !== 'GET' && req.method !== 'HEAD') {
    res.writeHead(405, security).end();
  } else if (path === '/') {
    const csp = `default-src 'self'; connect-src 'self' ${process.env.HEIMDALL_PUBLIC_URL_API ?? ''}; style-src 'self' 'unsafe-inline'`;
    res.writeHead(200, { ...security, 'content-type': 'text/html; charset=utf-8', 'content-security-policy': csp }).end(page);
  } else if (path === '/config.js') {
    res.writeHead(200, { ...security, 'content-type': 'text/javascript', 'cache-control': 'no-store' }).end(runtimeConfig());
  } else if (path === '/app.js') {
    readFile(new URL('./app.js', import.meta.url)).then(
      (js) => res.writeHead(200, { ...security, 'content-type': 'text/javascript' }).end(js),
      () => res.writeHead(500, security).end());
  } else {
    res.writeHead(404, security).end();
  }
});

server.listen(port, () => console.log(JSON.stringify({ event: 'web.listening', port })));
process.on('SIGTERM', () => server.close(() => process.exit(0)));
