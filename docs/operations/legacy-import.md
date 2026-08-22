# Legacy one-time import

The v2 loop does not synchronize with GitHub Issues. Cutover uses one bounded,
read-only export followed by a deterministic dry run:

```sh
gh issue list --state open --limit 1000 --json number,title,body,labels,comments > legacy-export.raw.json
# Normalize the GitHub response into agentic-loop/legacy-export/v1 outside the
# repository, then keep the potentially sensitive source outside Git.
devbox run -- go run ./cmd/legacy-import < legacy-export.v1.json > manifest.json
```

The command performs no network or canonical-state writes. Completed,
cancelled, duplicate, and superseded entries are excluded. Secret-like or
oversized content is quarantined by digest and reason; its title/body/comments
are not copied into the manifest. Review counts and digests before the later
transactional import/cutover command. Never commit either export or manifest.
