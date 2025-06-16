SHELL := /bin/bash
.SHELLFLAGS := -o pipefail -c
.DEFAULT_GOAL := help

ARCH          ?= $(shell go env GOARCH)
SERVICES      := catalog-api order-worker edge-gateway
SVC_VERSIONS  := 1.0.0 1.1.0
# Reproducible builds: no paths, no VCS stamp, no build id, fixed timestamp.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)
export SOURCE_DATE_EPOCH
GOBUILD       := CGO_ENABLED=0 go build -trimpath -buildvcs=false
LDFLAGS       := -s -w -buildid=
COMPOSE       := docker compose -f deploy/compose.yaml
OPS           := docker exec -i relorch-ops
OPS_T         := docker exec -it relorch-ops
REGISTRY_PUSH ?= http://localhost:5050
HOST_RELCTL   := dist/bin/relctl

.PHONY: help
help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-14s %s\n", $$1, $$2}'

.PHONY: test lint
test: ## unit tests
	go test ./...
lint: ## go vet + shellcheck
	go vet ./...
	go vet -tags integration ./test/integration
	shellcheck -s bash -x preflight/run.sh preflight/lib.sh preflight/checks/*.sh test/faults/*.sh scripts/*.sh

.PHONY: dist
dist: ## build relctl (host + linux) and every service version for linux/$(ARCH)
	$(GOBUILD) -ldflags "$(LDFLAGS)" -o $(HOST_RELCTL) ./cmd/relctl
	GOOS=linux GOARCH=$(ARCH) $(GOBUILD) -ldflags "$(LDFLAGS)" -o dist/linux/bin/relctl ./cmd/relctl
	@for v in $(SVC_VERSIONS); do for s in $(SERVICES); do \
	  GOOS=linux GOARCH=$(ARCH) $(GOBUILD) -ldflags "$(LDFLAGS) -X main.version=$$v" -o dist/build/$(ARCH)/$$s-$$v ./cmd/$$s || exit 1; \
	done; done
	@echo "built for linux/$(ARCH)"

.PHONY: images
images: dist ## build reproducible OCI images and push them to the local registry
	@for v in $(SVC_VERSIONS); do for s in $(SERVICES); do \
	  $(HOST_RELCTL) image build --binary dist/build/$(ARCH)/$$s-$$v --name $$s --version $$v --arch $(ARCH) \
	    --out dist/images --push $(REGISTRY_PUSH) || exit 1; \
	done; done

.PHONY: checksums
checksums: dist ## SHA256SUMS of every binary and image digest (what CI compares across runners)
	@for v in $(SVC_VERSIONS); do for s in $(SERVICES); do \
	  $(HOST_RELCTL) image build --binary dist/build/$(ARCH)/$$s-$$v --name $$s --version $$v --arch $(ARCH) --out dist/images >/dev/null || exit 1; \
	done; done
	@cd dist && { sha256sum build/$(ARCH)/* linux/bin/relctl; \
	  for f in images/*-$(ARCH).json; do jq -r '"\(.digest | ltrimstr("sha256:"))  image:\(.name):\(.version)"' $$f; done; } \
	  | LC_ALL=C sort -k2 > SHA256SUMS.$(ARCH)
	@cat dist/SHA256SUMS.$(ARCH)

keys/release.key:
	$(HOST_RELCTL) keygen --out keys/release --trust keys/trusted

.PHONY: keys
keys: dist keys/release.key ## generate the signing key (once) and trust it

.PHONY: up down
up: ## start the cluster, push images, bootstrap state and secrets
	$(COMPOSE) up -d --build
	$(MAKE) dist images keys
	./scripts/wait-cluster.sh
	$(OPS) relctl bootstrap

down: ## stop the cluster and delete its volumes
	$(COMPOSE) down -v --remove-orphans

VERSION     ?= 1.0.0
SVC_VERSION ?= 1.0.0
TARGET      ?= 0
.PHONY: release deploy
release: ## make and sign bundle releases/out/$(VERSION) (SVC_VERSION, TARGET=schema)
	$(HOST_RELCTL) manifest create --clean --spec releases/spec.json --version $(VERSION) --service-version $(SVC_VERSION) \
	  --arch $(ARCH) --schema-target $(TARGET) --out releases/out/$(VERSION)
	$(HOST_RELCTL) manifest sign --bundle releases/out/$(VERSION) --key keys/release.key

deploy: ## deploy releases/out/$(VERSION) from the ops box
	$(OPS) relctl deploy --bundle releases/out/$(VERSION)

.PHONY: status shell
status: ## what is running where
	$(OPS) relctl status
shell: ## shell on the ops box
	$(OPS_T) bash

.PHONY: faults recovery bench
faults: dist ## inject each preflight fault and check it is caught before rollout
	$(OPS) bash test/faults/run.sh

# Per-architecture path, so a binary built on another machine is never reused.
dist/linux/$(ARCH)/integration.test: $(shell find internal test -name '*.go')
	GOOS=linux GOARCH=$(ARCH) CGO_ENABLED=0 go test -c -tags integration -o $@ ./test/integration

recovery: dist dist/linux/$(ARCH)/integration.test ## run the recovery scenarios against the cluster
	$(OPS) dist/linux/$(ARCH)/integration.test -test.v -test.timeout 150m -test.run "$${RUN:-.}" 2>&1 | tee results/recovery.log

bench: dist ## time N auto-rollbacks of a crashing release
	$(OPS) bash scripts/bench-rollback.sh $${N:-10}
