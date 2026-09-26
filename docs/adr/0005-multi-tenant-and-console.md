# ADR 0005: Multi-tenant gateway.agentics.dk and the agentics.dk console

- Status: **Accepted** 2026-09-25
- Date: 2026-09-25
- Supersedes: the "no setup UI" line in PRD 0001 §7. A UI is now in scope, as a
  client of `/api/v1`.
- Related: [ADR 0002](0002-credential-sources.md), [ADR 0004](0004-runner-upstream-connectors.md),
  www-site `lib/vault/api.ts` (the pattern this copies)

## Context

The gateway was built single-owner: one `GATEWAY_OWNER` per deployment. Poul
decided on 2026-09-25 that it will be:
- **hosted multi-tenant** at `gateway.agentics.dk`;
- **configurable from agentics.dk** under `/u/<handle>`, next to Vault, for
  people who don't want to use the CLI.

That has to square with the platform's claim that **agentics.dk holds no
credentials**. The vault console already does:
- it forwards the viewer's own access token;
- it never handles plaintext;
- it projects every response.

## Decision

### Tenancy

1. **Every gateway entity lives under an Owner**:
   `owners/{owner}/gateway/{apis,mcp,bundles,principals,subscriptions,credentials,connectors,usage}`.
   This is the layout the store already has.

2. **Owners are claimed, the same way vault owners are.**
   - `POST /api/v1/owners {id}` creates `owners/{owner}/gateway/owner.json` with
     the caller's OIDC `sub` as the first admin.
   - An owner's admins manage everything under it: `PUT/DELETE /api/v1/owners/{owner}/admins/{sub}`.
   - The realm role `GatewayAdmin` remains a platform superuser.
   - The gateway does not consult the agentics.dk directory, so it stays
     self-contained and can be self-hosted.

3. **The owner is in every URL.** No lookup relies on a global id.
   - Management: `/api/v1/owners/{owner}/…`
   - Traffic: `/apis/{owner}/{api}/…` and `/mcp/{owner}/{server}`
   - OAuth metadata: `/.well-known/oauth-protected-resource/mcp/{owner}/{server}`
   - Connectors: the connector token identifies the owner (ADR 0004).

   Key formats are unchanged. A key is only checked against the owner named in
   the URL, so a valid key presented at another owner's path is simply invalid.

4. **Self-service stays sub-based.**
   - `GET /api/v1/me/subscriptions` spans all owners and returns
     `{owner, subscription, bundle}`.
   - A subscriber holding only a key can read what the key grants with
     `GET /apis/{owner}/_catalog` (key-authenticated). `gateway-cli sub env`
     works without an operator login.

5. **Legacy single-owner features stay** under `GATEWAY_OWNER`: the passthrough
   lane, the sim, OTEL and `/api/projects`. They are not multi-tenanted.

### Console (www-site `/u/<handle>/gateway`)

6. **A menu item beside Vault.** It is built as the vault console is:
   - **The viewer's own token is forwarded** server-to-server. There is no
     service account, and agentics.dk gains no standing privilege over any
     gateway owner.
   - **Every response is projected, never cast**, and the page is gated on the
     viewer being the handle's owner, as `requireVaultOwner` does.
   - **Parity rule:** the UI may not do anything `/api/v1` cannot. CLI and agents
     stay first-class.

7. **Credentials in the console never pass the Next server in plaintext.**
   - `runner` (ADR 0004): no credential at all. This is the preferred path for
     anything a customer can run.
   - `vault`: a reference `<owner>/<item>`. The secret is deposited into the
     vault through the existing browser-side deposit flow.
   - `sealed`: the browser encrypts to the gateway's **public** seal key (an
     X25519 sealed box; `GET /api/v1/seal-key`), and Next only relays
     ciphertext. This replaces the symmetric-only sealing for values that
     arrive over the API. `GATEWAY_SEAL_KEY` remains the at-rest key on the
     gateway.
   - `env` and `entra`: shown and referenced, but only a self-hoster can
     provision the values.

## Consequences

- The hosted gateway holds credentials only in two cases: sealed, and vault
  values cached for under an hour. Runner-hosted APIs hold none. The console
  holds none, ever.
- The URL shape changes before any release: `/apis/{api}` becomes
  `/apis/{owner}/{api}`. Nothing is deployed yet, so no compatibility shim is
  needed.
- The gateway's OIDC issuer must be the agentics realm, because www-site
  forwards the user's token. That realm is also the MCP authorization server.
