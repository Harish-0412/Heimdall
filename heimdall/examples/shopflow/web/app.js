// Storefront behaviour. window.SHOPFLOW comes from /config.js at runtime.
const { apiUrl, pr, sha } = window.SHOPFLOW;
document.getElementById('preview').textContent = pr ? `Preview of PR #${pr} (${sha})` : 'Local';

const price = (cents) => `$${(cents / 100).toFixed(2)}`;
const status = document.getElementById('status');

async function order(sku) {
  const res = await fetch(`${apiUrl}/api/orders`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ sku, quantity: 1, email: 'reviewer@example.com' }),
  });
  status.textContent = res.ok ? `Order placed for ${sku}.` : `Order failed (${res.status}).`;
}

async function load() {
  const list = document.getElementById('products');
  try {
    const res = await fetch(`${apiUrl}/api/products`);
    const products = await res.json();
    list.replaceChildren(...products.map((p) => {
      const li = document.createElement('li');
      const button = document.createElement('button');
      button.textContent = `Buy ${price(p.price_cents)}`;
      button.addEventListener('click', () => order(p.sku));
      li.append(p.name, button);
      return li;
    }));
  } catch (err) {
    list.replaceChildren(Object.assign(document.createElement('li'), { textContent: `Cannot reach the API: ${err.message}` }));
  }
}

load();
