-- Synthetic seed data for ShopFlow previews. Never load production data here:
-- previews are reviewed by many people and run pull-request code.
--
-- Heimdall loads this file after migrations, as the unprivileged app role, in
-- a single transaction, into the baseline database that `reset` restores.

INSERT INTO products (sku, name, price_cents) VALUES
  ('SF-TEE-001',  'Heimdall T-shirt',          2500),
  ('SF-MUG-002',  'Bifrost coffee mug',        1400),
  ('SF-CAP-003',  'Asgard baseball cap',       1900),
  ('SF-STK-004',  'Preview environment stickers', 500),
  ('SF-HOO-005',  'Gjallarhorn hoodie',        5400)
ON CONFLICT (sku) DO NOTHING;

INSERT INTO orders (product_id, quantity, email)
SELECT id, 2, 'reviewer@example.com' FROM products WHERE sku = 'SF-MUG-002';
