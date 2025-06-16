# id: PF16
# name: No long transactions or table locks
# category: database
# needs: PF08
# A migration's DDL would queue behind these and block every query after it.
check() {
  local trx mdl
  trx=$(mysql_q information_schema "SELECT CONCAT('thread ', trx_mysql_thread_id, ' open ', TIMESTAMPDIFF(SECOND, trx_started, NOW()), 's, ', trx_rows_locked, ' rows locked') FROM information_schema.innodb_trx WHERE trx_started < NOW() - INTERVAL $LONG_TRX_SECONDS SECOND") || {
    echo "cannot read innodb_trx (needs PROCESS)"
    return 1
  }
  mdl=$(mysql_q performance_schema "SELECT CONCAT(OBJECT_NAME, ' ', LOCK_TYPE) FROM performance_schema.metadata_locks WHERE OBJECT_SCHEMA = '$APP_DB' AND LOCK_STATUS = 'GRANTED' AND LOCK_TYPE IN ('SHARED_NO_READ_WRITE', 'SHARED_READ_ONLY', 'SHARED_NO_WRITE', 'EXCLUSIVE')") || {
    echo "cannot read metadata_locks"
    return 1
  }
  if [ -n "$trx$mdl" ]; then
    echo "blocking: $(echo "$trx" "$mdl" | paste -sd ';' -)"
    return 1
  fi
  echo "no transaction older than ${LONG_TRX_SECONDS}s, no table locks"
}
