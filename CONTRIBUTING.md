# Contributing to sr-cache

Thanks for your interest. This is a small focused project; PRs that match the
project's scope (caching, breaker, observability, deployment, docs) are
welcome.

## Project scope

The proxy does exactly four things:

1. Caches Schema Registry responses with route-aware TTLs.
2. Coalesces concurrent identical misses via singleflight.
3. Trips a circuit breaker on upstream rate-limits and transport errors.
4. Forwards a verified client identity header for use behind an mTLS Ingress.

Things this project explicitly does **not** do (PRs adding them will be
declined unless we agree on scope first):

- TLS termination on the proxy (use an Ingress)
- OAuth2 / JWT verification (use an Ingress)
- Per-CN rate limiting (use the Ingress's rate-limit layer)
- Schema introspection / transformation (the proxy is byte-for-byte
  transparent to clients)
- Apicurio / AWS Glue Schema Registry support (Confluent-only)
- Stale-while-revalidate cache strategy

## Dev setup

You'll need:

- Go 1.25+
- Docker (for the reference Valkey container in local testing)

```bash
git clone https://github.com/akrishnanDG/sr-cache
cd sr-cache

cp .env.example .env
# fill in UPSTREAM_URL, UPSTREAM_API_KEY, UPSTREAM_API_SECRET

# Start a local Valkey:
docker run -d --rm --name sr-cache-valkey -p 6379:6379 valkey/valkey:8-alpine

make test       # run unit tests
make run        # run the proxy locally on :8080
```

Run `make help` for the full set of dev commands.

## Tests

- Unit tests live alongside the code (`internal/<pkg>/<file>_test.go`).
- Integration via `miniredis` for the cache layer + `httptest` for the
  upstream client — no live SR needed for the test suite.
- Run with `make test`; the CI pipeline runs `make ci` (vet + tests).
- Add a regression test for any bug you fix.

## Commit conventions

- One conceptual change per commit.
- Imperative subject line, under 72 chars (e.g. `Fix breaker deadlock on
  transport errors`).
- Body explains *why*, not *what* — the diff already shows what.

## Pull requests

- Open against `main`.
- Keep PRs small and focused; if you find yourself touching 10+ files,
  consider whether it should be multiple PRs.
- Include a short description of why the change is needed and how you
  tested it.
- CI must be green.
- Internal data (real customer names, live API keys, internal ticket IDs,
  proprietary endpoints) must never appear in commits — see `.gitignore`.

## Reporting bugs

Open a GitHub issue with:
- What you expected
- What happened
- Steps to reproduce (config snippets, request shapes)
- Logs (sanitized of any secrets)

## License

By contributing you agree your contributions are licensed under the
Apache License 2.0, same as the rest of the project.
