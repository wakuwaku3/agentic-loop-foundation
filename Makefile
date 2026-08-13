.DEFAULT_GOAL := check

.PHONY: environment format lint test check

environment:
	./scripts/check-environment.sh

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./tests/run-e2e.sh

check: environment lint test
