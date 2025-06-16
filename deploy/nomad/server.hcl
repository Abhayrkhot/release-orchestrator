datacenter = "dc1"
data_dir   = "/nomad/data"
bind_addr  = "0.0.0.0"
log_level  = "INFO"

advertise {
  http = "{{ GetInterfaceIP \"eth0\" }}"
  rpc  = "{{ GetInterfaceIP \"eth0\" }}"
  serf = "{{ GetInterfaceIP \"eth0\" }}"
}

server {
  enabled          = true
  bootstrap_expect = 1
  # Notice a lost node within ~15s instead of the default ~30s.
  min_heartbeat_ttl = "5s"
  heartbeat_grace   = "10s"
}

client {
  enabled = false
}
