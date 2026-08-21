.DEFAULT_GOAL := check

.PHONY: environment format lint test check smoke affected affected-audit

environment:
	./scripts/check-environment.sh

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./tests/run-e2e.sh

check: environment lint test

smoke:
	./bin/agentic-loop smoke

# local affected check: gateではない。編集中のfeedback短縮専用（docs/policies/
# validation-harness.md、docs/decisions/0021-affected-check-selection.md）。
affected:
	./scripts/affected-check.sh

affected-audit:
	./scripts/affected-check.sh --audit
