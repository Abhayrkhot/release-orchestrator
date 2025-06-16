#!/usr/bin/env bash
# Helpers shared by every preflight check. Sourced, never executed.
# shellcheck shell=bash

: "${NOMAD_ADDR:=http://nomad-server:4646}"
: "${REGISTRY_URL:=http://registry:5000}"
: "${MYSQL_HOST:=mysql}"
: "${MYSQL_PORT:=3306}"
: "${MYSQL_USER:=relctl}"
: "${MYSQL_PASSWORD:=}"
: "${APP_DB:=app}"
: "${STATE_DB:=relctl}"
: "${REQUIRED_NODES:=3}"
: "${RELCTL:=relctl}"
: "${HTTP_TIMEOUT:=3}"
: "${LONG_TRX_SECONDS:=10}"
: "${CURRENT_MANIFEST:=}"
: "${CURRENT_VERSION:=}"
: "${ALLOW_DOWNGRADE:=0}"
: "${CURRENT_UNKNOWN:=0}"
: "${TRUST_DIR:=/etc/relctl/trusted}"
: "${BUNDLE_DIR:=$(dirname "$MANIFEST")}"
: "${SIGNATURE:=$MANIFEST.sig}"

# mysql_q DB SQL: tab-separated rows, no header.
mysql_q() {
  MYSQL_PWD="$MYSQL_PASSWORD" mysql --protocol=TCP -h "$MYSQL_HOST" -P "$MYSQL_PORT" -u "$MYSQL_USER" \
    --connect-timeout=3 --batch --skip-column-names -e "$2" "$1"
}

# table_exists DB TABLE
table_exists() {
  [ "$(mysql_q "$1" "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = '$1' AND TABLE_NAME = '$2'")" = 1 ]
}

nomad_get() { curl -fsS --max-time "$HTTP_TIMEOUT" "$NOMAD_ADDR$1"; }

# services: names in the release being deployed.
services() { jq -r '.services[].name' "$MANIFEST"; }
current_services() { [ -n "$CURRENT_MANIFEST" ] && jq -r '.services[].name' "$CURRENT_MANIFEST"; }

# svc_field MANIFEST SERVICE JQ_EXPR
svc_field() { jq -r --arg s "$2" ".services[] | select(.name == \$s) | $3" "$1"; }

sha256() { sha256sum "$1" | cut -d' ' -f1; }
