.DEFAULT_GOAL := check

.PHONY: environment format lint test check affected affected-audit

environment:
	./scripts/check-environment.sh

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./tests/test-stage-evidence.sh
	./tests/run-e2e.sh

check: environment lint test

# local affected check: gateではない。編集中のfeedback短縮専用（docs/policies/
# validation-harness.md、docs/decisions/0021-affected-check-selection.md）。
affected:
	./scripts/affected-check.sh

affected-audit:
	./scripts/affected-check.sh --audit
