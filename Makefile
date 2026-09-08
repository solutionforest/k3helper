BINARY     := k3helper
VERSION    := 0.1.0
VERPKG     := github.com/solutionforest/k3helper/internal/cli.version
LDFLAGS    := -ldflags "-X $(VERPKG)=$(VERSION)"
# -s -w strips the symbol table and DWARF: ~25% smaller downloads, and Go
# panics keep their function names because the runtime carries its own tables.
RELFLAGS   := -ldflags "-s -w -X $(VERPKG)=$(VERSION)"
PLATFORMS  := linux/amd64 linux/arm64 darwin/arm64 darwin/amd64
DIST       := dist
TARGETS    := test/sandbox/targets.sandbox.yaml
SSH_KEY    := test/sandbox/ssh/id_ed25519
SSH_OPTS   := -i $(SSH_KEY) -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes

# Sandbox nodes are OrbStack Linux VMs (NOT Docker containers — OrbStack
# containers share the macOS kernel and kubelet PLEG kills pods falsely).

.PHONY: help build build-all test test-integration clean e2e e2e-fast e2e-quick \
        sandbox-up sandbox-down sandbox-reset sandbox-verify sandbox-ssh \
        bootstrap check doctor fault-clean fault-list fault-check-all release release-upload bundle

help:
	@echo "Build:"
	@echo "  make build            build k3helper for current platform"
	@echo "  make build-all        cross-compile (linux/darwin amd64+arm64)"
	@echo "  make test             unit tests"
	@echo "  make test-integration integration tests (needs sandbox)"
	@echo "  make e2e              full lifecycle E2E (resets sandbox, ~10-15 min)"
	@echo "  make e2e-fast         E2E reusing running sandbox (~8 min)"
	@echo "  make e2e-quick        E2E without the fault sweep (~4 min)"
	@echo "  make clean            remove bin/ and dist/"
	@echo "Distribution:"
	@echo "  make release          stripped binaries + checksums.txt in dist/"
	@echo "  make release-upload   create/refresh the v$(VERSION) GitHub release and upload dist/"
	@echo "  make bundle           offline paste bundle for air-gapped web-console installs"
	@echo "Sandbox (3 Ubuntu 24.04 OrbStack VMs):"
	@echo "  make sandbox-up       create VMs + install SSH + write targets"
	@echo "  make sandbox-down     delete VMs"
	@echo "  make sandbox-reset    fresh VMs"
	@echo "  make bootstrap        install k3s on all nodes via k3helper"
	@echo "  make check            run k3helper check"
	@echo "  make doctor           run k3helper doctor"
	@echo "  make fault-list       list faults + the signature each should trigger"
	@echo "  make fault-<name>     inject one fault (e.g. make fault-oom)"
	@echo "  make fault-check-all  inject every fault, assert doctor catches each"
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
# make e2e-fast       reuse running sandbox (~8 min)
# make e2e-quick      reuse sandbox, skip the fault sweep (~4 min)
e2e:
	./test/e2e.sh

e2e-fast:
	./test/e2e.sh --keep

# Skips the multi-fault sweep as well; the shortest useful loop.
e2e-quick:
	./test/e2e.sh --keep --quick

# Release artifacts consumed by install.sh: one binary per platform plus a
# checksums.txt the installer verifies the download against.
release:
	@rm -rf $(DIST) && mkdir -p $(DIST)
	@for platform in $(PLATFORMS); do \
	  os=$${platform%/*}; arch=$${platform#*/}; \
	  GOOS=$$os GOARCH=$$arch go build $(RELFLAGS) -o $(DIST)/$(BINARY)-$$os-$$arch ./cmd/k3helper || exit 1; \
	  echo "✓ $(DIST)/$(BINARY)-$$os-$$arch"; \
	done
	@cd $(DIST) && (command -v sha256sum >/dev/null && sha256sum $(BINARY)-* || shasum -a 256 $(BINARY)-*) > checksums.txt
	@cp install.sh $(DIST)/install.sh
	@echo "✓ $(DIST)/checksums.txt"

release-upload: release
	@gh release view v$(VERSION) >/dev/null 2>&1 \
	  || gh release create v$(VERSION) --title "v$(VERSION)" --notes "k3helper v$(VERSION)"
	gh release upload v$(VERSION) $(DIST)/$(BINARY)-* $(DIST)/checksums.txt $(DIST)/install.sh --clobber
	@echo "✓ uploaded to https://github.com/solutionforest/k3helper/releases/tag/v$(VERSION)"

# For servers with no outbound internet: PLATFORM=, COMPRESS=, CHUNK_LINES=
bundle:
	@VERSION=$(VERSION) scripts/bundle.sh

clean:
	rm -rf bin $(DIST)

sandbox-up:
	@test/sandbox/setup-orbstack.sh
	@$(MAKE) --no-print-directory _write-targets sandbox-verify

_write-targets:
	@S=$$(orb -m sandbox-server hostname -I | awk '{print $$1}'); \
	A1=$$(orb -m sandbox-agent1 hostname -I | awk '{print $$1}'); \
	A2=$$(orb -m sandbox-agent2 hostname -I | awk '{print $$1}'); \
	printf 'cluster: sandbox\nnodes:\n  - name: server\n    role: server\n    host: %s\n    port: 22\n    user: sandbox\n    key: test/sandbox/ssh/id_ed25519\n    insecure_host_key: true\n  - name: agent1\n    role: agent\n    host: %s\n    port: 22\n    user: sandbox\n    key: test/sandbox/ssh/id_ed25519\n    insecure_host_key: true\n  - name: agent2\n    role: agent\n    host: %s\n    port: 22\n    user: sandbox\n    key: test/sandbox/ssh/id_ed25519\n    insecure_host_key: true\n' $$S $$A1 $$A2 > $(TARGETS)

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

# Fault injection. `make fault-list` shows every fault and the doctor
# signature it is expected to trigger.
fault-list:
	@test/faults/fault.sh list

fault-%:
	@test/faults/fault.sh $*

fault-clean:
	@test/faults/fault.sh clean

# The troubleshooter's exam: inject each fault, assert doctor diagnoses it.
fault-check-all:
	@test/faults/check-all.sh
