# ADR 0003: Access model: Bundle + Subscription

- Status: **Accepted** 2026-09-23 (names: Bundle, Subscription, Principal)
- Date: 2026-09-23
- Context doc: [PRD 0001](../prd/0001-api-gateway.md) (F8–F12)

## Context

APIM grants access through **Products** (a group of APIs) and **Subscriptions**
(a named container for two keys, owned by a user and scoped to a product, an API,
or all APIs). The new AI Gateway tier drops that in favour of gateway-wide
runtime keys, which reach every model and tool. Poul wants per-API assignment,
"assign people to the API", so the classic model wins.

"Product" is the wrong word here. It collides with the agentics.dk products page,
with product-cli's graph ("the product"), and with how we talk about our own
catalogue.

## Decision

- An **API** or **MCP server** is published through a **Bundle** (APIM's
  Product).
- A **Principal** (a human OIDC `sub`, or a service principal for an agent)
  holds a **Subscription** to exactly **one** Bundle.
- A Subscription has two keys, primary and secondary, each shown once and stored
  as sha256. The key format is `gwk_<subscriptionId>_<secret>`, following the
  pks-agent-azure / pks-agent-coolify `cra_` layout, so the lookup is O(1) by id
  and the comparison runs in constant time.
- States: `active` → `suspended` ⇄ `active`; `cancelled` is terminal; `expired`
  is derived from `expiresAt`.
- In v1 the scope is Bundle only. API-scoped and all-API subscriptions are
  dropped for subscribers; operators use OIDC on the management plane instead.
- A request is authorized when the key resolves to an `active` subscription whose
  Bundle contains the API or MCP server being called.
- Unpublishing (hiding) a Bundle hides it from the catalog but **does not**
  invalidate keys, as in APIM. Removing an API from a Bundle does cut access.

## Names to decide

| Concept | Recommended | Alternatives | Why the recommendation |
|---|---|---|---|
| APIM *Product* | **Bundle** | Plan, Pack, Offering | Neutral and concrete ("the speech bundle"). "Plan" implies pricing tiers, and we have no billing. |
| APIM *Subscription* | **Subscription** | Pass, Access, Seat | Familiar to anyone who knows APIM. The Azure and Stripe collision is tolerable within this product. "Pass" reads well in the CLI (`gateway-cli pass create`). |
| APIM *User* | **Principal** | Consumer, Member | Covers humans and agents equally. |

Whatever wins, the rename changes labels in the PRD, the graph and the CLI verbs.
The model itself stays the same.

## Consequences

- One Subscription per Bundle means a person with two bundles has two keys. That
  is simple to revoke, but it is two env lines. A "multi-bundle key" is Later if
  it turns out to hurt.
- The existing `members.json` / project model becomes legacy. PRD open question 5
  decides whether "project" survives as a label.
