# Contributing

## Development setup

1. Install Go 1.23 or newer.
2. Copy `.env.example` to `.env` and set a unique `ADMIN_PASSWORD`.
3. Run the HTTP API and WebSocket relay from the repository root so both load the same `.env` and SQLite path.

## Go conventions

- Run `gofmt` on Go changes.
- Keep command packages focused on dependency wiring and server lifecycle; keep reusable code in `internal` packages.
- Add comments for package purpose, exported declarations, lifecycle behavior, and decisions that are not obvious from code. Avoid comments that merely restate a line.
- Return errors to the command entry point; do not terminate the process from reusable packages.
- Keep authentication and authorization checks server-side. Screen capture must retain explicit Android system consent.
- Update `docs/` when changing the API, WebSocket messages, or deployment behavior.

## Checks

```sh
go test ./...
go vet ./...
```
