# Security Policy

## Current security model

AI Wiki is designed for a single trusted user or a trusted private network. **It has no authentication, users, roles or access control.**

- Anyone who can reach the HTTP port can read, create, edit, restore and delete articles, comments and attachments, and download the full JSON export (`/api/export`).
- The MCP endpoint (`/mcp`) and REST API (`/api/*`) are equally unauthenticated.
- Author names are free-text labels supplied by the client; they are not verified identities, and the audit journal and history cannot prove who made a change.
- The server listens on `127.0.0.1:8090` by default. Every request must have a loopback `Host` or the host and effective port configured with `-allowed-origin`; other hosts are rejected to guard against DNS rebinding. `X-Forwarded-Host` is not trusted.
- Browser requests carrying an `Origin` header must match the configured origin exactly. For a loopback request host, matching loopback origins and HTTP loopback origins on ports 8090, 5173, 4173 and 18088 are also accepted for development. This development exception does not apply to remote request hosts. Treat other local applications using those ports as trusted too.
- These Host/Origin guards are **not** authentication: clients can choose their Host and omit Origin. Anyone with network access can still read and change data using an accepted Host. Use a VPN or an authenticating proxy to restrict access.
- There is no TLS, rate limiting or per-user quota built in. Attachments are limited to 25 MB per request.
- Data (SQLite database, attachments, backups) is stored unencrypted on disk.

Do not expose the server to the internet or an untrusted network. If you must publish it beyond loopback, put it behind a VPN or a reverse proxy that provides TLS and authentication, and set `-allowed-origin` to the exact external origin.

A reverse proxy must accept only the configured public server name and preserve the client's Host (including a non-default port). Rewriting every Host to the loopback backend would disable the remote-host distinction and enable the loopback development-origin exception for proxied requests. Do not rely on forwarded headers for this guard.

PostgreSQL, SSO and ACL support are roadmap items only and are not part of the current code.

## Supported versions

Only the latest commit on the default branch is supported. There are no versioned releases yet.

## Reporting a vulnerability

Please report vulnerabilities privately rather than in public issues or pull requests.

- If the repository has GitHub private vulnerability reporting enabled, use the "Report a vulnerability" button on the repository's **Security** tab.
- Otherwise, contact the maintainer through the contact details on the maintainer's GitHub profile and ask for a private channel before sharing details. Do not post exploit details publicly.

Please include affected commit, reproduction steps, impact and any suggested fix. This is a small project maintained on a best-effort basis; no response time is guaranteed.

Reports that only state "there is no authentication" are already documented above; reports of ways to bypass the origin check, path traversal in attachments, injection, or unsafe rendering of Markdown are in scope.
