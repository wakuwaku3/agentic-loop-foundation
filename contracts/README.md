# Contracts

Versioned JSON Schema contracts are the stable boundary for the v2 control
plane. Every document uses `schema_version: "v1"`, rejects unknown fields, and
contains references to large content rather than embedding provider or
credential material. `schemas/` contains the canonical contracts,
`fixtures/valid` and `fixtures/invalid` contain executable examples, and
`openapi/openapi-v1.yaml` is the HTTP API contract. Run `make contracts` to
parse every schema and validate every fixture.

Increment decomposition is deliberately absent from the public API: humans
submit requirements, while the loop scheduler performs planning internally.
