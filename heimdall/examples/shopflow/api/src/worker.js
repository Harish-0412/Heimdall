// Notifications worker: consumes order events and marks orders as notified.
// (A real worker would send an email; previews must never send real ones.)
import pg from 'pg';
import amqp from 'amqplib';
import { required, QUEUE, log } from './env.js';

const db = new pg.Pool({ connectionString: required('DATABASE_URL'), max: 2 });
const broker = await amqp.connect(required('AMQP_URL'));
const channel = await broker.createChannel();
await channel.assertQueue(QUEUE, { durable: true });
await channel.prefetch(10);

await channel.consume(QUEUE, async (msg) => {
  if (msg === null) return;
  try {
    const { orderId } = JSON.parse(msg.content.toString());
    await db.query('UPDATE orders SET notified_at = now() WHERE id = $1', [orderId]);
    log('notification.sent', { orderId });
    channel.ack(msg);
  } catch (err) {
    log('notification.failed', { message: err.message });
    channel.nack(msg, false, false);
  }
});
log('worker.ready', { queue: QUEUE });

for (const signal of ['SIGTERM', 'SIGINT']) {
  process.on(signal, async () => {
    log('worker.stopping', { signal });
    await Promise.allSettled([channel.close(), broker.close(), db.end()]);
    process.exit(0);
  });
}
