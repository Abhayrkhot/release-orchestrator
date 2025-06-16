# id: PF08
# name: MySQL reachable
# category: database
check() {
  local v
  v=$(mysql_q "$APP_DB" "SELECT VERSION()" 2>&1) || { echo "cannot connect to $MYSQL_HOST:$MYSQL_PORT: $(echo "$v" | tail -1)"; return 1; }
  echo "MySQL $v at $MYSQL_HOST:$MYSQL_PORT"
}
