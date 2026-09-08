BINARY     := k3helper
VERSION    := 0.1.0
LDFLAGS    := -ldflags "-X github.com/solutionforest/k3helper/internal/cli.version=$(VERSION)"
TARGETS    := test/sandbox/targets.sandbox.yaml
SSH_KEY    := test/sandbox/ssh/id_ed25519
SSH_OPTS   := -i $(SSH_KEY) -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes

# Sandbox nodes are OrbStack Linux VMs (NOT Docker containers — OrbStack
# containers share the macOS kernel and kubelet PLEG kills pods falsely).

.PHONY: help build build-all test test-integration clean e2e \
        sandbox-up sandbox-down sandbox-reset sandbox-verify sandbox-ssh \
        bootstrap check doctor fault-clean

help:
	@echo "Build:"
	@echo "  make build            build k3helper for current platform"
	@echo "  make build-all        cross-compile (linux/darwin amd64+arm64)"
	@echo "  make test             unit tests"
	@echo "  make test-integration integration tests (needs sandbox)"
	@echo "  make e2e              full lifecycle E2E (resets sandbox, ~10-15 min)"
	@echo "  make e2e-fast         E2E reusing running sandbox (~6 min)"
	@echo "  make clean            remove bin/"
	@echo "Sandbox (3 Ubuntu 24.04 OrbStack VMs):"
	@echo "  make sandbox-up       create VMs + install SSH + write targets"
	@echo "  make sandbox-down     delete VMs"
	@echo "  make sandbox-reset    fresh VMs"
	@echo "  make bootstrap        install k3s on all nodes via k3helper"
	@echo "  make check            run k3helper check"
	@echo "  make doctor           run k3helper doctor"
	@echo "  make fault-clean      undo injected faults"

build:
	go build $(LDFLAGS) -o bin/$(BINARY) ./cmd/k3helper

build-all:
	@for platform in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64; do \
	  GOOS=$${platform%/*} GOARCH=$${platform#*/} go build $(LDFLAGS) \
	    -o bin/$(BINARY)-$${platform%/*}-$${platform#*/} ./cmd/k3helper; \
	  echo "✓ bin/$(BINARY)-$${platform%/*}-$${platform#*/}"; \
	done

test:
	go test ./...

test-integration:
	go test -tags=integration ./...

# Full lifecycle E2E: sandbox→bootstrap→check→deploy→fault→doctor→recover.
# make e2e            full run incl. fresh sandbox reset (~10-15 min)
# make e2e-fast       reuse running sandbox (~6 min)
e2e:
	./test/e2e.sh

e2e-fast:
	./test/e2e.sh --keep

clean:
	rm -rf bin

sandbox-up:
	@test/sandbox/setup-orbstack.sh
	@$(MAKE) --no-print-directory _write-targets sandbox-verify

_write-targets:
	@S=$$(orb -m sandbox-server hostname -I | awk '{print $$1}'); \
	A1=$$(orb -m sandbox-agent1 hostname -I | awk '{print $$1}'); \
	A2=$$(orb -m sandbox-agent2 hostname -I | awk '{print $$1}'); \
	printf 'cluster: sandbox\nnodes:\n  - name: server\n    role: server\n    host: %s\n    port: 22\n    user: sandbox\n    key: test/sandbox/ssh/id_ed25519\n  - name: agent1\n    role: agent\n    host: %s\n    port: 22\n    user: sandbox\n    key: test/sandbox/ssh/id_ed25519\n  - name: agent2\n    role: agent\n    host: %s\n    port: 22\n    user: sandbox\n    key: test/sandbox/ssh/id_ed25519\n' $$S $$A1 $$A2 > $(TARGETS)

sandbox-down:
	-orb delete sandbox-server --force 2>/dev/null
	-orb delete sandbox-agent1 --force 2>/dev/null
	-orb delete sandbox-agent2 --force 2>/dev/null

sandbox-reset: sandbox-down sandbox-up

sandbox-verify:
	@fail=""; \
	for ip in $$(grep 'host:' $(TARGETS) | awk '{print $$2}'); do \
	  if ssh $(SSH_OPTS) -o ConnectTimeout=5 sandbox@$$ip 'echo ok' 2>/dev/null | grep -q ok; then \
	    echo "✓ ssh $$ip"; else echo "✗ ssh $$ip FAILED"; fail=1; fi; \
	done; [ -z "$$fail" ]

sandbox-ssh:
	@orb -m $(or $(NODE),sandbox-server) sudo su -

bootstrap:
	go run ./cmd/k3helper vm setup -t $(TARGETS) \
	  --server-extra-args "--snapshotter=native --disable=traefik" \
	  --agent-extra-args "--snapshotter=native" \
	  --kubeconfig sandbox-kubeconfig.yaml

check:
	go run ./cmd/k3helper check -t $(TARGETS)

doctor:
	go run ./cmd/k3helper doctor -t $(TARGETS)

fault-clean:
	@for ip in $$(grep 'host:' $(TARGETS) | awk '{print $$2}'); do \
	  ssh $(SSH_OPTS) sandbox@$$ip 'sudo rm -f /bigfile /tmp/oom-pod*.yaml 2>/dev/null; sudo systemctl start k3s k3s-agent 2>/dev/null' 2>/dev/null; \
	done; echo faults cleaned
