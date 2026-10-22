# Contributing to Trequila

Thanks for helping. Bug reports, fixes, docs and focused features are all welcome.

## Before you start

- **Small change** (bug fix, docs, test): open a pull request directly.
- **Larger change** (new endpoint, new dependency, structural change): open an issue first so we can agree on
  the approach. The direction of the project is in
  [`docs/design/2026-10-22-trequila-refocus-design.md`](docs/design/2026-10-22-trequila-refocus-design.md); changes
  that fit its roadmap move fastest.
- **Security issue**: do not open a public issue. Follow [`SECURITY.md`](SECURITY.md).

By contributing you agree that your work is licensed under the [Apache 2.0 license](LICENSE) and that you will
follow the [Code of Conduct](CODE_OF_CONDUCT.md).

## Set up

You need Go 1.25+ and Docker with the Compose plugin.

```bash
git clone https://github.com/Sky-walkerX/Trequila.git && cd Trequila
make env      # .env with generated local secrets (never committed)
make up       # API + Postgres + NATS JetStream + Redis on localhost
make logs     # follow the API
make check    # gofmt + go vet + go test (Postgres-backed tests skip)
make test-int # everything, including the tests that need Postgres
```

Run the API from your editor instead of the container by starting only the dependencies
(`docker compose up -d db nats redis`) and exporting the same variables the `api` service uses in
[`docker-compose.yml`](docker-compose.yml). Host ports are `55432` (Postgres), `54222` (NATS) and `56379` (Redis), all bound to `127.0.0.1`; the
Postgres password is `POSTGRES_PASSWORD` in your `.env`.

`init.sql` is the canonical schema and is applied on the first start of an empty database volume. Run
`make reset` to start from scratch.

## Making a change

1. Branch from `main`: `git switch -c fix/short-description`.
2. Keep the change focused. Unrelated cleanup belongs in its own pull request.
3. Add or update tests for the behaviour you change, and watch a new test fail before you make it pass. Use
   real Postgres through `internal/testdb` (each test gets a throwaway schema; it skips when
   `TEST_DATABASE_URL` is unset) rather than mocking the database, and the in-memory bus
   (`internal/bus/memory`) instead of a broker.
4. Update [`API_DOCUMENTATION.md`](API_DOCUMENTATION.md) in the same change when a route is added, removed or
   reshaped.
5. Run `make test-int` and `golangci-lint run`. CI runs both, with the race detector, and also applies and
   rolls back every migration.
6. Open a pull request describing what changed and why, and how you verified it.

### Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/): `type(scope): summary`, imperative mood,
no trailing period, under about 72 characters. Types: `feat`, `fix`, `refactor`, `perf`, `test`, `docs`,
`build`, `ci`, `chore`. Explain the *why* in the body when it is not obvious.

```
fix(stream): complete the SSE stream when places never arrive
```

## Code standards

- Format with `gofmt`; `make fmt` fails if anything is unformatted.
- Prefer the standard library. Add a dependency only when it clearly earns its place, and say why in the pull
  request.
- Construct LLM providers through `internal/llm.NewProvider` and `Close()` every one you create.
- Schema changes are goose migrations in `migrations/` with a working `Down`; never edit one that has shipped.
- Event handlers are delivered at least once, so make them idempotent; read ADRs 001-004 in `docs/adr` first.
- Anything that takes a URL from a user goes through `internal/netguard`.
- Never log or return a plaintext API key. Keys exist in memory only, and are stored encrypted.
- Do not weaken the startup checks on `JWT_SECRET` and `ENCRYPTION_KEY`.
- Handle errors explicitly. A swallowed error that turns into a `null` column is a bug here.

## Reporting bugs

Use the bug report template. Include what you ran, what you expected, what happened, and the relevant log
lines (with secrets removed).
