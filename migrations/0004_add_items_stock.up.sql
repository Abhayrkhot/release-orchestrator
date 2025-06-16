-- Expand-only: a nullable column with a default, so services built for
-- schema 3 keep working after this runs.
ALTER TABLE items ADD COLUMN stock INT NOT NULL DEFAULT 0;
CREATE INDEX idx_items_stock ON items (stock);
