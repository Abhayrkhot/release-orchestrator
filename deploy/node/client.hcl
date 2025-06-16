datacenter = "dc1"
data_dir   = "/var/lib/nomad"
bind_addr  = "0.0.0.0"
log_level  = "INFO"

advertise {
  http = "{{ GetInterfaceIP \"eth0\" }}"
  rpc  = "{{ GetInterfaceIP \"eth0\" }}"
  serf = "{{ GetInterfaceIP \"eth0\" }}"
}

client {
  enabled           = true
  servers           = ["nomad-server:4647"]
  network_interface = "eth0"
  # ARM VMs expose no CPU clock speed, so Nomad would fingerprint 0 MHz and
  # place nothing. Each node gets a fixed 4000 MHz budget.
  cpu_total_compute = 4000
}

plugin "docker" {
  config {
    allow_privileged = false
    volumes {
      enabled = false
    }
    # Keep old images: a rollback should not depend on the registry.
    gc {
      image       = true
      image_delay = "24h"
      container   = true
    }
  }
}
