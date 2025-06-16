# id: PF19
# name: No interrupted release
# category: release
# needs: PF08
check() {
  table_exists "$STATE_DB" releases || { echo "no release history"; return 0; }
  local rows
  rows=$(mysql_q "$STATE_DB" "SELECT CONCAT('release ', version, ' (id ', id, ') stopped at ', stage) FROM releases WHERE status = 'in_progress'")
  if [ -n "$rows" ]; then
    echo "$(echo "$rows" | paste -sd ';' -); run relctl reconcile first"
    return 1
  fi
  echo "no release in progress"
}
