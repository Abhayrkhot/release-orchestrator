# Where relctl, the Bash preflights and the tests run: a Linux box on the
# cluster network with the mysql client, jq, openssl, curl and the nomad CLI.
FROM mysql:8.4.13
RUN microdnf install -y jq openssl procps-ng findutils && microdnf clean all
COPY --from=hashicorp/nomad:2.1.1 /bin/nomad /usr/local/bin/nomad
COPY --from=docker:29.10.0-dind /usr/local/bin/docker /usr/local/bin/docker
ENV PATH=/work/dist/linux/bin:$PATH
ENTRYPOINT []
CMD ["sleep", "infinity"]
