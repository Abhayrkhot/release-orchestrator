-- Local development credentials only.
CREATE DATABASE IF NOT EXISTS app;
CREATE DATABASE IF NOT EXISTS relctl;

-- The controller: owns relctl, runs migrations on app, and may read
-- transaction and lock state for the preflight checks.
CREATE USER 'relctl'@'%' IDENTIFIED BY 'relctlpw';
GRANT ALL PRIVILEGES ON app.* TO 'relctl'@'%';
GRANT ALL PRIVILEGES ON relctl.* TO 'relctl'@'%';
GRANT PROCESS ON *.* TO 'relctl'@'%';
GRANT SELECT ON performance_schema.* TO 'relctl'@'%';

-- The services: data access only, no DDL.
CREATE USER 'app'@'%' IDENTIFIED BY 'apppw';
GRANT SELECT, INSERT, UPDATE, DELETE ON app.* TO 'app'@'%';
