CREATE TABLE items (
  sku         VARCHAR(64) PRIMARY KEY,
  name        VARCHAR(255) NOT NULL,
  price_cents INT NOT NULL,
  created_at  DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
);
INSERT INTO items (sku, name, price_cents) VALUES
  ('SKU-1', 'Widget', 1299),
  ('SKU-2', 'Gadget', 2499),
  ('SKU-3', 'Gizmo', 999);
