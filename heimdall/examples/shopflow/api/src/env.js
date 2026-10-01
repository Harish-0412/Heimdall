// Environment injected by Heimdall (see docs/rendering.md). Failing fast on a
// missing variable gives a clear termination message, which Heimdall's
// diagnostics read, instead of a confusing error later.
export function required(name) {
  const value = process.env[name];
  if (!value) {
    console.error(`missing required environment variable ${name}`);
    process.exit(1);
  }
  return value;
}

export const QUEUE = 'orders.created';

export function log(event, fields = {}) {
  console.log(JSON.stringify({ time: new Date().toISOString(), event, ...fields }));
}
