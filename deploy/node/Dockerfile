# A Nomad client with its own Docker daemon: one "Linux node" of the cluster.
# Debian rather than the Alpine-based docker:dind image, because the Nomad
# release binary is linked against glibc. Docker's binaries are static, so
# they are copied from the official dind image unchanged.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends iptables ca-certificates curl procps pigz xz-utils e2fsprogs \
 && rm -rf /var/lib/apt/lists/*
COPY --from=docker:29.10.0-dind /usr/local/bin/ /usr/local/bin/
COPY --from=hashicorp/nomad:2.1.1 /bin/nomad /usr/local/bin/nomad
COPY client.hcl /etc/nomad.d/client.hcl
COPY entrypoint.sh /usr/local/bin/node-entrypoint.sh
ENV DOCKER_TLS_CERTDIR=""
VOLUME /var/lib/docker
ENTRYPOINT ["/usr/local/bin/node-entrypoint.sh"]
