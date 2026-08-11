.DEFAULT_GOAL := check

.PHONY: environment format lint test check

environment:
	./scripts/check-environment.sh

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./tests/test-agentic-loop.sh

check: environment lint test
