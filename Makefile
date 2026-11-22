BIN := bin
PYTHON ?= python3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMENT_PYTHON := .venv/comment-lint/bin/python
COMMENT_DEPS := .venv/comment-lint/requirements.stamp
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet lint comment-lint-env comment-lint comment-lint-e2e race fmt check-commit install-hooks python-build python-e2e typescript typescript-e2e clean

build: agent
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/sandboxd ./cmd/sandboxd
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/sandboxctl ./cmd/sandboxctl

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

python-e2e:
	PYTHON=$(PYTHON) bash scripts/e2e-python.sh artifacts/python-e2e

clean:
	rm -rf $(BIN)

typescript:
	cd sandbox-ts && npm ci && npm run build && npm run check

typescript-e2e:
	bash scripts/e2e-typescript.sh artifacts/typescript-e2e

.PHONY: agent
agent: internal/dockerbackend/guestbin/guest-linux-amd64 internal/dockerbackend/guestbin/guest-linux-arm64

internal/dockerbackend/guestbin/guest-linux-%: $(wildcard sandbox-agent/cmd/guest/*.go internal/guestprotocol/*.go sandbox-agent/go.*)
	cd sandbox-agent && CGO_ENABLED=0 GOOS=linux GOARCH=$* go build -trimpath -ldflags='-s -w' -o ../$@ ./cmd/guest
