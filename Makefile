BIN := bin
PYTHON ?= python3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARTIFACTS ?= artifacts
COMMENT_PYTHON := .venv/comment-lint/bin/python
COMMENT_DEPS := .venv/comment-lint/requirements.stamp
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet lint comment-lint-env comment-lint comment-lint-e2e race fmt check-commit install-hooks python-build python-e2e typescript typescript-e2e clean

build: agent
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/execletd ./cmd/execletd
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/execletctl ./cmd/execletctl

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

lint: comment-lint
	golangci-lint run ./...

$(COMMENT_DEPS): scripts/comment-lint/requirements.txt
	$(PYTHON) -m venv .venv/comment-lint
	$(COMMENT_PYTHON) -m pip install -r $<
	@touch $@

comment-lint-env: $(COMMENT_DEPS)

comment-lint: comment-lint-env
	$(COMMENT_PYTHON) scripts/comment-lint/check.py $(if $(COMMENT_BASE),--base "$(COMMENT_BASE)")

comment-lint-e2e: comment-lint-env
	$(COMMENT_PYTHON) scripts/comment-lint/e2e.py artifacts/comment-lint/e2e.json

fmt:
	gofmt -l -w .

check-commit:
	./scripts/check-commit-msg.sh "HEAD~1..HEAD"

install-hooks:
	git config core.hooksPath .githooks

python-build:
	$(PYTHON) -m build --outdir artifacts/python-dist sandbox-python
	$(PYTHON) scripts/check-sdk-wheel.py artifacts/python-dist/execlet-*.whl

python-e2e:
	PYTHON=$(PYTHON) bash scripts/e2e-python.sh artifacts/python-e2e

clean:
	rm -rf $(BIN)

docs-openapi:
	$(PYTHON) scripts/sync-docs-openapi.py

docs-check:
	$(PYTHON) scripts/sync-docs-openapi.py --check

typescript:
	cd sandbox-ts && npm ci && npm run build && npm run check

typescript-e2e:
	bash scripts/e2e-typescript.sh artifacts/typescript-e2e

.PHONY: agent
agent: internal/dockerbackend/guestbin/guest-linux-amd64 internal/dockerbackend/guestbin/guest-linux-arm64

internal/dockerbackend/guestbin/guest-linux-%: $(wildcard sandbox-agent/cmd/guest/*.go internal/guestprotocol/*.go sandbox-agent/go.*)
	cd sandbox-agent && GOFLAGS= CGO_ENABLED=0 GOOS=linux GOARCH=$* go build -trimpath -ldflags='-s -w' -o ../$@ ./cmd/guest

.PHONY: feature-e2e runtime-e2e runtime-image volumes-e2e docs-openapi docs-check

feature-e2e: build typescript
	$(PYTHON) scripts/e2e-mcp.py $(ARTIFACTS)/mcp-e2e
	$(PYTHON) scripts/e2e-guest.py --samples 2 --out $(ARTIFACTS)/guest
	$(PYTHON) scripts/e2e-guest-faults.py --out $(ARTIFACTS)/guest-faults
	$(PYTHON) scripts/e2e-guest-boundaries.py --out $(ARTIFACTS)/guest-boundaries
	$(PYTHON) scripts/guest-transport-probe.py --out $(ARTIFACTS)/guest-transport
	$(PYTHON) scripts/e2e-terminal.py --out $(ARTIFACTS)/terminal
	$(PYTHON) scripts/e2e-terminal-sdks.py --out $(ARTIFACTS)/terminal-sdks
	$(PYTHON) scripts/e2e-guest-idle.py --server bin/execletd --out $(ARTIFACTS)/guest-idle/acceptance.json
	$(PYTHON) scripts/e2e-output-retention.py --out $(ARTIFACTS)/output-retention
	$(PYTHON) scripts/egress-e2e/run.py --out $(ARTIFACTS)/egress
	$(PYTHON) scripts/e2e-templates.py --out $(ARTIFACTS)/templates
	$(PYTHON) scripts/e2e-prewarm.py --samples 2 --out $(ARTIFACTS)/prewarm-e2e

runtime-image:
	docker build --target minimal -t execlet-guest:minimal -f examples/guest/Dockerfile.agent-minimal examples/guest

runtime-e2e: build runtime-image
	$(PYTHON) scripts/e2e-runtime.py --out $(ARTIFACTS)/runtime
	$(PYTHON) scripts/e2e-volumes.py --out $(ARTIFACTS)/volumes
	$(PYTHON) scripts/e2e-templates.py --runtime runsc --out $(ARTIFACTS)/runsc-template

volumes-e2e: build runtime-image
	$(PYTHON) scripts/e2e-volumes.py --out $(ARTIFACTS)/volumes
