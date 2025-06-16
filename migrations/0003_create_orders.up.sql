CREATE TABLE orders (
  id             BIGINT AUTO_INCREMENT PRIMARY KEY,
  sku            VARCHAR(64) NOT NULL,
  quantity       INT NOT NULL,
  worker_version VARCHAR(32) NOT NULL,
  created_at     DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_orders_sku (sku)
);
