CREATE TABLE service_events (
  id              BIGINT AUTO_INCREMENT PRIMARY KEY,
  service         VARCHAR(64) NOT NULL,
  version         VARCHAR(32) NOT NULL,
  release_version VARCHAR(32) NOT NULL DEFAULT '',
  alloc_id        VARCHAR(64) NOT NULL,
  node            VARCHAR(64) NOT NULL,
  event           VARCHAR(16) NOT NULL,
  at              DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_service_event (service, event, at)
);
