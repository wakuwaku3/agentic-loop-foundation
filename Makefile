.DEFAULT_GOAL := check

.PHONY: format lint test check

format:
	./scripts/format.sh

lint:
	./scripts/lint.sh

test:
	./tests/test-agentic-loop.sh

check: lint test
