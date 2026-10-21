# Contributing

Thanks for your interest in AI Wiki. The project is a Go + SQLite server with a React + TypeScript frontend, intended for a trusted network. Corporate features (PostgreSQL, SSO, ACL) are roadmap only and are not implemented.

## Requirements

- Go 1.25+
- Node.js 22.12+ and npm
- Docker is optional (only for the compose files)

No private server, account or AI model is needed to build or test.

## Build and test

```sh
go test ./... -count=1
go vet ./...
cd web
npm ci
npm run build
cd ..
node scripts/qa.mjs
```

`node scripts/qa.mjs` needs `web/dist` (built above) and `go` in `PATH`. It starts a temporary server on port 18091 with its own database under `.cache/` and does not touch `data/`. See [docs/VERIFICATION.md](docs/VERIFICATION.md).

For development run `go run ./cmd/aiwiki` and, in `web/`, `npm run dev` (Vite proxies the API to port 8090).

## Pull requests

- Keep changes focused; describe what changed and why.
- Add or update tests for behavior changes. Run the commands above and report what you ran and what failed, if anything.
- Do not commit data, databases, backups, `.env` files, tokens or personal addresses. Use documentation addresses (for example `192.0.2.0/24`, `example.org`) in examples and tests.
- Keep revision-conflict (`base_revision`, HTTP 409) semantics intact; see [docs/CONTRACT.md](docs/CONTRACT.md).
- Documentation should describe current behavior, not planned features, unless clearly marked as roadmap.

## Security issues

Do not open public issues for vulnerabilities. See [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contributions are licensed under the MIT License (see [LICENSE](LICENSE)).
