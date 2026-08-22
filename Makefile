.PHONY: check environment format lint test contracts docs secrets smoke clean infra-policy infra-lint infra-validate component-plan affected candidate candidate-affected component ownership component-ci component-contracts component-control-plane component-runner component-domain component-application component-store-memory component-store-firestore component-api component-web component-docs component-test component-infra component-tooling

GO ?= go
EVIDENCE_DIR ?= build/evidence
BASE ?= HEAD^

check: environment format lint test contracts docs secrets ownership component-store-firestore

component-plan:
	@go run ./cmd/ci-plan --changed "$$(scripts/affected.sh --list "$(BASE)")"

affected:
	@scripts/affected.sh "$(BASE)"

ownership:
	@go run ./cmd/ci-plan --tracked "$$(git ls-files --cached --others --exclude-standard | paste -sd, -)"

component:
	@test -n "$(COMPONENT)"
	@go run ./cmd/ci-plan --execute --component "$(COMPONENT)" --evidence-out "$(EVIDENCE_DIR)"

candidate:
	@go run ./cmd/ci-plan --candidate --evidence-dir "$(EVIDENCE_DIR)"

candidate-affected:
	@go run ./cmd/ci-plan --candidate --candidate-changed "$$(scripts/affected.sh --list "$(BASE)")" --evidence-dir "$(EVIDENCE_DIR)"

component-ci:
	@go test ./internal/ci ./cmd/ci-plan
component-contracts:
	@go test ./internal/contracts
component-control-plane:
	@go test ./cmd/control-plane ./internal/api ./internal/domain
component-runner:
	@go test ./cmd/runner ./cmd/bootstrap ./internal/domain ./internal/runner
component-domain:
	@go test ./internal/domain
component-application:
	@go test ./internal/application
component-store-memory:
	@go test ./internal/store/memory
component-store-firestore:
	@scripts/firestore-emulator.sh go test -race ./internal/store/firestore
component-api:
	@go test ./internal/api
component-web:
	@go test ./internal/web
component-docs:
	@! rg -n 'TODO\(ci\)' docs README.md
component-test:
	@go test ./...
component-infra:
	@scripts/infra-policy.sh
	@if command -v tofu >/dev/null 2>&1; then scripts/infra-validate.sh; else echo 'tofu not installed; policy-only infra check'; fi
component-tooling:
	@true

# All targets are read-only checks except clean, which is intentionally local.
environment:
	@command -v $(GO) >/dev/null || (echo "Go is required" >&2; exit 1)
	@$(GO) version
	@test "$$(git rev-parse --show-toplevel)" = "$$(pwd)"

format:
	@test -z "$$(gofmt -l .)" || (echo "Go files require gofmt" >&2; gofmt -l .; exit 1)

lint:
	@$(GO) vet ./...

test:
	@$(GO) test ./...

contracts:
	@$(GO) test ./internal/contracts

docs:
	@! rg -n '\]\([^)]*01-core-principles\.md' docs README.md AGENTS.md
	@! rg -n '\]\([^)]*(00-product-definition|02-user-facing-spec|03-release-and-documentation|04-current-feature-inventory|05-domain-model|06-logical-architecture|08-technology-selection|09-validation-strategy|10-implementation-and-migration|11-documentation-system|13-v2-work-orchestration)\.md' docs README.md AGENTS.md

secrets:
	@gitleaks git --no-banner --redact

smoke:
	@$(GO) run ./cmd/runner --version | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+-dev$$'
	@$(GO) run ./cmd/bootstrap --version | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+-dev$$'
	@port=18080; PORT=$$port $(GO) run ./cmd/control-plane >/tmp/agentic-loop-v2-healthz.log 2>&1 & pid=$$!; trap 'kill $$pid 2>/dev/null || true' EXIT; for i in 1 2 3 4 5; do curl -fsS "http://127.0.0.1:$$port/healthz" && exit 0; sleep 1; done; exit 1

infra-policy:
	@scripts/infra-policy.sh

infra-lint:
	@scripts/infra-lint.sh

infra-validate:
	@scripts/infra-validate.sh

clean:
	@true
