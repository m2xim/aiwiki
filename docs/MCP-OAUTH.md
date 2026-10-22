# MCP OAuth deployment and client compatibility

**Assessment date: 2026-10-22.** This guide describes the optional MCP OAuth resource metadata and audience-binding path. It is not a claim of full MCP protocol compliance: this server still negotiates MCP version `2025-03-26` and makes no latest-specification conformance claim. A legacy client that only does same-origin discovery (authorization server on the MCP origin) may not support an external authorization server.

## Enable the OAuth resource

Set `AIWIKI_MCP_RESOURCE_URL` to the canonical public MCP resource URL, for example:

```text
AIWIKI_MCP_RESOURCE_URL=https://wiki.example.org/mcp
```

Use the exact externally visible HTTPS URL, including `/mcp`, and keep it on the same origin as the configured OIDC redirect URL. The value comes from deployment configuration; it is never derived from the incoming `Host` or `Forwarded` headers. Configure the reverse proxy and public DNS/TLS to serve this URL.

When enabled, the server advertises RFC 9728 protected-resource metadata. The strict RFC 9728 location is the path-based one, `/.well-known/oauth-protected-resource/mcp`; the root `/.well-known/oauth-protected-resource` document is a compatibility alias for clients that probe the origin root. The document identifies this configured resource, lists the existing OIDC issuer as its authorization server, and advertises only the `aiwiki:mcp` scope and Bearer header authentication. When disabled, those metadata endpoints return 404. See [RFC 9728](https://www.rfc-editor.org/rfc/rfc9728.html).

### Introspection credentials

| Setting | Environment variable | Meaning |
| --- | --- | --- |
| `MCPOAuthConfig.IntrospectionClientID` | `AIWIKI_MCP_INTROSPECTION_CLIENT_ID` | Confidential client that introspects MCP OAuth tokens. |
| `MCPOAuthConfig.IntrospectionClientSecret` | `AIWIKI_MCP_INTROSPECTION_CLIENT_SECRET` | Its secret. |

Both are optional and must be supplied as a pair; supplying either requires `AIWIKI_MCP_RESOURCE_URL`. If omitted, the legacy behavior applies and the browser OIDC client credentials are used. The browser introspection wrapper always keeps the browser credentials; only the check against the expected MCP resource audience uses the separate credentials when configured.

Why: Keycloak 26.8's `AccessTokenIntrospectionProvider.verifyAudience` ([source, lines 288-315](https://raw.githubusercontent.com/keycloak/keycloak/26.8.0/services/src/main/java/org/keycloak/protocol/oidc/AccessTokenIntrospectionProvider.java)) requires the introspecting confidential client's `clientId` to be in the token `aud`. With Resource Indicators the audience is the resource URI only, so the introspecting client must be a separate confidential client whose `clientId` is that URI. This also means the browser credentials cannot introspect an MCP token as active.

## Token and client behavior

| Deployment / credential | Audience and scope | Behavior |
| --- | --- | --- |
| OAuth resource URL unset | Existing browser OIDC client ID audience | Legacy MCP OIDC validation remains in effect. |
| OAuth resource URL set; external MCP OAuth token | Exact canonical `AIWIKI_MCP_RESOURCE_URL` audience **and** `aiwiki:mcp` scope | Signature, issuer, expiry and live introspection/admission checks must pass. There is no browser-client-ID audience fallback. |
| Browser sign-in callback | Browser OIDC client ID audience | Remains browser-client-bound; enabling MCP OAuth does not change the callback audience. |
| Native personal access token or temporary key | Its existing exact native scope and validation | Unchanged; these credentials do not become OAuth tokens. |

The scope `aiwiki:mcp` grants the user's full current content ACL. It does not mean read-only access and does not restrict access to selected projects. For narrowed grants (selected projects, read-only ceiling) use a native key (project-scoped PAT); see [persistent MCP access](MCP-PERSISTENT-ACCESS.md) and [temporary MCP access](MCP-TEMPORARY-ACCESS.md). Browser session credentials remain separate from MCP OAuth credentials; both resolve to the same local user when issuer and subject match.

External OAuth tokens are checked through OIDC introspection on MCP requests, subject to the identity provider's token lifetime, introspection behavior and directory synchronization/cache. This does not promise immediate visibility of an AD change. Native persistent credentials use their configured admission provider; its upstream directory freshness has the same deployment dependency.

**Limits.** There is no OAuth-specific per-request audit and no per-token rate limit beyond the existing browser-bearer behavior. Every MCP request introspects at the identity provider, so a pilot must measure proxy and IdP load. CORS / browser-JavaScript MCP clients are not supported.

## Keycloak setup

Keycloak is the authorization server. Its authorization-code flow handles PKCE S256, issuer identification, refresh and client registration. aiwiki is the protected resource: it does not broker OAuth, proxy Dynamic Client Registration (DCR), or hold OAuth client refresh tokens.

Three kinds of client are involved:

1. **Browser client** (confidential): unchanged, keeps its own-audience mapper and does **not** get `resource_url`.
2. **MCP resource client** (confidential, introspection only): `clientId` equal to the exact resource URL (for example `https://wiki.example.org/mcp`), a dedicated secret, attribute `resource_url` set to the same URI. Disable standard flow, implicit flow, direct access grants and service accounts, and give it no default or optional scopes. Pass its credentials as `AIWIKI_MCP_INTROSPECTION_CLIENT_ID` / `AIWIKI_MCP_INTROSPECTION_CLIENT_SECRET`.
3. **MCP user clients** (one dedicated, pre-registered **public** client per tool): authorization-code flow, PKCE S256 and that client's exact callback URL. Do not reuse the browser's confidential client or expose its secret.

Add the optional client scope `aiwiki:mcp`, include it in access tokens, and attach an Audience mapper to it whose **Included Client Audience** is the MCP resource client (the resource URL). The audience must appear in access tokens and introspection responses, not just ID tokens. Preserve the realm access/admission claims that aiwiki already requires (explicit `sub` and realm-role mappers on the user clients; do not rely on `roles`/`profile` default scopes that the realm may not define). Verify the actual realm configuration and permissions before rollout.

With the Resource Indicators feature on, Keycloak's [MCP guide](https://www.keycloak.org/securing-apps/mcp-authz-server) `resource_url` attribute plus the included client audience bind the access token to the resource URI. The guide also marks Resource Indicators and Client ID Metadata Documents (CIMD) experimental. Confirm the client sends the `resource` parameter and test the realm's resulting access-token and introspection claims. Avoid anonymous DCR and broad callback or origin allowlists.

**Production consent.** Set `consentRequired` to `true` on MCP user clients in production. The test fixture sets it to `false` only so the login can be scripted.

Apply these settings in a separate test realm before configuring the production identity provider.

## What was tested

| Area | Status as of 2026-10-22 |
| --- | --- |
| Actual authorization server, automated | The `TestMCPOAuth` integration test runs against a disposable Keycloak 26.8.0 (`--features=resource-indicators`) imported from `internal/server/testdata/keycloak-oauth-realm.json` (CI `corporate` job): discovery, PKCE S256 authorization-code login, resource-bound token, audience/scope/introspection acceptance, browser credentials rejected for the MCP token, and negative PKCE, resource and code-replay cases. Whether a given CI run passed is recorded by that run, not by this document. |
| Real Claude Code login | **Not executed.** |
| Real Codex CLI login | **Not executed.** |
| Real AD federation / S3 pilot | **Not done.** |
| ChatGPT web or Claude web over remote HTTPS | **Pilot/configuration required.** See [OpenAI's OAuth guidance](https://developers.openai.com/plugins/build/auth). |

### Pre-registered CLI examples (help output only)

Flags below were checked against `--help` of Codex 0.160.1 and Claude Code 2.1.301. **The login itself was not run.**

```text
codex mcp add aiwiki --url https://wiki.example.org/mcp --oauth-client-id aiwiki-codex
codex mcp login aiwiki

claude mcp add --transport http --client-id aiwiki-claude --callback-port 58082 aiwiki https://wiki.example.org/mcp
```

For Claude Code the callback is `http://localhost:58082/callback`, but this is version dependent: register the exact value the client prints. The Codex callback is not documented here; take it from the client's own output rather than guessing. See the [Claude Code MCP guide](https://code.claude.com/docs/en/mcp) and the [OpenAI MCP/CLI guide](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).

## Deployment and verification checklist

- [ ] Set the canonical public URL and confirm it matches the configured OIDC redirect origin and ends in `/mcp`.
- [ ] Create the separate MCP resource client and set the introspection environment variables as a pair.
- [ ] Configure the proxy to preserve the expected public `Host`; do not rely on `Forwarded` or `X-Forwarded-Host` to set the resource identity.
- [ ] With the feature enabled, confirm the path-based RFC 9728 metadata (and the root alias) advertise the configured resource, existing OIDC issuer and only `aiwiki:mcp`. With it disabled, confirm the metadata endpoints return 404.
- [ ] In the real Keycloak realm, confirm PKCE S256, exact callback allowlists, issuer response identification, optional `aiwiki:mcp` scope, resource audience in access and introspection responses, consent required, and normal realm access claims.
- [ ] Reject an external MCP token missing either the exact resource audience or `aiwiki:mcp`; confirm a browser-client-ID audience does not fall back, and that browser credentials cannot introspect the MCP token as active.
- [ ] Exercise the browser callback and native PAT/temporary-key flows separately to confirm their existing audiences and exact scopes remain unchanged.
- [ ] Verify content access against current ACLs and explicitly test the intended permission ceiling before giving an OAuth client access to production content.
- [ ] Run a pilot to measure proxy and IdP introspection load.

The matrix is a deployment assessment, not a conformance certification. Successful metadata discovery alone does not prove that a client completes login or that the selected Keycloak realm grants the intended permissions.
