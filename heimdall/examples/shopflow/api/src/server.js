// ShopFlow API: products from PostgreSQL (cached in Redis), orders published
// to RabbitMQ for the notifications worker. Everything it needs arrives as
// environment variables injected by Heimdall.
import http from 'node:http';
import pg from 'pg';
import { createClient } from 'redis';
import amqp from 'amqplib';
import { required, QUEUE, log } from './env.js';

const port = Number(required('PORT'));
const allowedOrigin = process.env.HEIMDALL_PUBLIC_URL_WEB ?? '*';

const db = new pg.Pool({ connectionString: required('DATABASE_URL'), max: 5 });
const cache = createClient({ url: required('REDIS_URL') });
cache.on('error', (err) => log('redis.error', { message: err.message }));
await cache.connect();
const broker = await amqp.connect(required('AMQP_URL'));
const channel = await broker.createConfirmChannel();
await channel.assertQueue(QUEUE, { durable: true });

function send(res, status, body) {
  res.writeHead(status, {
    'content-type': 'application/json',
    'access-control-allow-origin': allowedOrigin,
    'access-control-allow-methods': 'GET, POST, OPTIONS',
    'access-control-allow-headers': 'content-type',
    'x-content-type-options': 'nosniff',
  });
  res.end(body === undefined ? '' : JSON.stringify(body));
}

async function readJSON(req) {
  let body = '';
  for await (const chunk of req) {
    body += chunk;
    if (body.length > 16 * 1024) throw new Error('request body too large');
  }
  return JSON.parse(body || '{}');
}

const routes = {
  'GET /health': async () => {
    await db.query('SELECT 1');
    await cache.ping();
    await channel.checkQueue(QUEUE);
    return [200, { status: 'ok', pr: process.env.HEIMDALL_PR, sha: process.env.HEIMDALL_SHA }];
  },
  'GET /api/products': async () => {
    const cached = await cache.get('products');
    if (cached) return [200, JSON.parse(cached)];
    const { rows } = await db.query('SELECT id, sku, name, price_cents FROM products ORDER BY id');
    await cache.set('products', JSON.stringify(rows), { EX: 10 });
    return [200, rows];
  },
  'GET /api/orders': async () => {
    const { rows } = await db.query(
      'SELECT id, product_id, quantity, email, notified_at, created_at FROM orders ORDER BY id DESC LIMIT 20');
    return [200, rows];
  },
  'POST /api/orders': async (req) => {
    const { sku, quantity = 1, email } = await readJSON(req);
    if (typeof sku !== 'string' || typeof email !== 'string' || !Number.isInteger(quantity) || quantity < 1) {
      return [400, { error: 'sku, email and a positive integer quantity are required' }];
    }
    const { rows } = await db.query(
      `INSERT INTO orders (product_id, quantity, email)
       SELECT id, $2, $3 FROM products WHERE sku = $1 RETURNING id`, [sku, quantity, email]);
    if (rows.length === 0) return [404, { error: `unknown sku ${sku}` }];
    channel.sendToQueue(QUEUE, Buffer.from(JSON.stringify({ orderId: rows[0].id })), { persistent: true });
    await channel.waitForConfirms();
    log('order.created', { orderId: rows[0].id });
    return [201, { id: rows[0].id }];
  },
};

const server = http.createServer(async (req, res) => {
  if (req.method === 'OPTIONS') return send(res, 204);
  const route = routes[`${req.method} ${new URL(req.url, 'http://x').pathname}`];
  if (!route) return send(res, 404, { error: 'not found' });
  try {
    const [status, body] = await route(req);
    send(res, status, body);
  } catch (err) {
    log('request.failed', { path: req.url, message: err.message });
    send(res, 500, { error: 'internal error' });
  }
});

server.listen(port, () => log('api.listening', { port }));

for (const signal of ['SIGTERM', 'SIGINT']) {
  process.on(signal, () => {
    log('api.stopping', { signal });
    server.close(async () => {
      await Promise.allSettled([channel.close(), broker.close(), cache.quit(), db.end()]);
      process.exit(0);
    });
  });
}
