# id: PF09
# name: Migration user privileges
# category: database
# needs: PF08
check() {
  local grants line p missing=()
  grants=$(mysql_q "$APP_DB" "SHOW GRANTS FOR CURRENT_USER()") || { echo "SHOW GRANTS failed"; return 1; }
  line=$(echo "$grants" | grep -F "ON \`$APP_DB\`.*" | head -1)
  if echo "$line" | grep -q "ALL PRIVILEGES"; then
    echo "$MYSQL_USER has ALL PRIVILEGES on $APP_DB"
    return 0
  fi
  for p in SELECT INSERT UPDATE DELETE CREATE ALTER INDEX DROP; do
    echo "$line" | grep -Eq "(GRANT |, )$p(,| ON)" || missing+=("$p")
  done
  if [ ${#missing[@]} -gt 0 ]; then
    echo "$MYSQL_USER lacks ${missing[*]} on $APP_DB; migrations would fail part way"
    return 1
  fi
  echo "$MYSQL_USER can run migrations on $APP_DB"
}
