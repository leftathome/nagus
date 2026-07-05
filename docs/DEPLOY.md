# Deploying nagus

nagus runs as a single long-lived `nagus serve` process behind a read-only
surface (`/mcp`, `/search`, `/item`, `/watches`) with an optional in-process
ingest loop. It ships as the `charts/nagus` Helm chart. This guide covers a
quick local install, the gitops (Flux) deployment used on orac, storage
backends, secrets, and how openclaw consumes it.

> **Single-writer:** with the sqlite backend the chart runs **one** replica
> (`Recreate` strategy) -- the same pod ingests and serves, so there is no
> cross-pod file sharing. Do not scale it up on sqlite.

## Endpoints

All on the Service, port 8080:

| path | purpose |
|---|---|
| `/mcp` | MCP server (JSON-RPC 2.0), tools `search_items` + `get_item` -- the agent-facing surface openclaw registers |
| `/search`, `/item` | plain-HTTP read-only pull (debug / non-MCP callers) |
| `/watches` | per-watch candidates + strong matches for the delivery cron |
| `/healthz`, `/readyz` | probes |

Everything is **read-only** (eyes, not hands): non-GET is rejected; no mutating
tool exists.

## Quick local install (self-contained demo)

No secrets, no network -- ingests a bundled fixture and scores it offline:

```
helm install nag charts/nagus --set demo.enabled=true
kubectl port-forward svc/nag-nagus 8080:8080
curl -s -X POST localhost:8080/mcp -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_items","arguments":{"limit":5}}}'
```

## Gitops deployment (Flux, orac)

Mirror the glovebox pattern. Add, under the GitOps repo:

- `clusters/orac/sources/helmrepo-nagus.yaml` -- an OCI `HelmRepository` at
  `oci://ghcr.io/leftathome/charts`.
- `clusters/orac/apps/nagus/namespace-nagus.yaml`,
  `helmrelease-nagus.yaml` (references `chart: nagus`, the pinned `version`, and
  the `nagus` HelmRepository), and `kustomization.yaml`.
- a `- nagus/` entry in `clusters/orac/apps/kustomization.yaml`.

Sketch of the HelmRelease values:

```yaml
spec:
  chart:
    spec:
      chart: nagus
      version: "0.1.0"
      sourceRef: { kind: HelmRepository, name: nagus, namespace: flux-system }
  values:
    category: hdd            # or land
    storage:
      backend: sqlite        # or postgres (see below)
    serve:
      ingestInterval: "30m"  # 0 disables scheduled ingest
```

Roll a new version by bumping `version` to the released tag.

## Storage backends

### sqlite (default)

A PVC-backed single file at `/data/nagus.db`. Zero external dependencies. Tune
`storage.sqlite.persistence.{size,storageClass}`.

### postgres (shared CloudNativePG)

`storage.backend: postgres` targets `postgres-rw.databases-app.svc.cluster.local`.
The chart does **not** create the cluster/database -- provisioning is a gitops
request in `clusters/orac/foundation/databases-app/` (drafted; see beads):

1. add a managed role `nagus` to `cluster-postgres.yaml`,
2. add `database-nagus.yaml` (`kind: Database`, owner `nagus`),
3. add `externalsecret-nagus-role.yaml` (Vault `eso/nagus/infrastructure`,
   property `postgres_password`).

**Prerequisite:** put `postgres_password` in Vault at
`secret/data/eso/nagus/infrastructure` first. A pod cannot mount the
`databases-app` secret cross-namespace, so the chart syncs the same Vault key
into a local `<release>-db` secret in its own namespace
(`storage.postgres.externalSecret.enabled`, default on).

pgvector is not in the shared image; v1 postgres is FTS-only.

## Secrets

Never commit credentials. All come from Vault via external-secrets, split into
two KV paths by tier: **`eso/nagus/infrastructure`** (platform/DB, also read by
the gitops CNPG role in `databases-app`) and **`eso/nagus/sources`** (the app's
outbound source-API creds -- one secret, one property per source).

| k8s secret | Vault path -> property | keys exposed | consumed via |
|---|---|---|---|
| Postgres role (`<release>-db`) | `eso/nagus/infrastructure` -> `postgres_password` | `username`, `password` | `storage.postgres.externalSecret` (on by default) |
| Rentcast (land) | `eso/nagus/sources` -> `rentcast_key` | `NAGUS_RENTCAST_KEY` | `land.rentcastExternalSecret` (or `land.rentcastSecret` override) |
| eBay OAuth (hdd live ingest) | `eso/nagus/sources` -> `ebay_client_id`, `ebay_client_secret` | `NAGUS_EBAY_CLIENT_ID`, `NAGUS_EBAY_CLIENT_SECRET` | `externalSecret` (or `ebay.existingSecret` override) |

The demo path (`demo.enabled=true`) and the keyless Craigslist **land** source
need no secrets at all.

## eBay API call budget

eBay production access is capped at ~5,000 calls/day (License 2.4). The connector
counts every OAuth + search call against a per-UTC-day budget
(`NAGUS_EBAY_DAILY_BUDGET`, default 5000) and, when present, honors a
rate-remaining response header. On exhaustion it backs off until the next UTC day
(logged, not errored) and **never rotates keys or otherwise circumvents the cap**.
`GET /metrics` exposes `nagus_ebay_api_calls_{budget,used,remaining}`
(Prometheus text). Size `serve.ingestInterval` so daily ingests stay under budget.

## eBay content freshness (License 8.1(b))

Stored eBay listings must be deleted once no longer public and displayed data
must be < 6h older than eBay. After each ingest the hdd pipeline purges eBay
items not re-seen within `EbayContentMaxAge` (6h): live listings are re-ingested
(their `SeenAt` refreshed), stale/ended ones fall past the cutoff and are removed.
**Set `serve.ingestInterval` well under 6h** (e.g. 30m-1h) so live listings are
refreshed before the window closes. The Craigslist **land** source is not eBay
Content and is not purged.

### Sandbox testing

To validate against the real eBay APIs without spending the production budget,
use the eBay **Sandbox** (License 8.4, a separate test environment):
`NAGUS_EBAY_SANDBOX=true` routes the connector to `api.sandbox.ebay.com`. The
opt-in integration test is build-tagged so the default `go test ./...` stays
offline:

```
NAGUS_EBAY_SANDBOX_CLIENT_ID=... NAGUS_EBAY_SANDBOX_CLIENT_SECRET=... \
  go test -tags ebayintegration ./internal/connector/ebay/
```

Sandbox Application Keys live in Vault at `eso/nagus/testing`, under the **same
keys** as production `sources` (`ebay_client_id`, `ebay_client_secret`) -- export
them to `NAGUS_EBAY_SANDBOX_CLIENT_ID` / `_SECRET` for the test. Never publish PII
or restricted data to the sandbox.

### Seller profile enrichment (optional, off by default)

`NAGUS_EBAY_SELLER_PROFILE=true` enables a per-seller second API call (eBay
Shopping `GetUserProfile`) that adds `seller_account_age_tier` and
`seller_recent_sales_tier` to items. It is **off by default** because each
distinct seller costs one budgeted call. The seller username is used only as a
transient lookup argument (never stored); results are cached per fetch and taken
as a fresh snapshot each run. **Validate the `GetUserProfile` field mapping
against the sandbox / live keyset (nagus-hm0) before enabling in production.**

## Categories

- **hdd**: eBay source + `$/TB` valuation. Live ingest needs eBay credentials +
  `serve.ingestInterval`. nagus stores NO eBay user PII (no seller username or
  per-seller key); only coarse, per-listing seller-trust tiers land on the item,
  which is why we take the Marketplace Account Deletion opt-out. See SECURITY.md.
- **land**: Craigslist source (`land.craigslistCity`, `land.craigslistCategory`)
  + structure-first scoring. Set `land.budgetCents` / `land.minAcreageAcres` /
  `land.maxAcreageAcres`; enable `land.rentcastExternalSecret` (syncs the key
  from `eso/nagus/sources`) or set `land.rentcastSecret` for structure signals
  (without it, land surfaces as unassessed candidates). The Craigslist source is
  keyless; it needs residential egress -- in-cluster egress qualifies.

## Watches (delivery)

Set `watches` in values (or mount a JSON config via `NAGUS_WATCHES`) -- each is a
saved query + threshold. openclaw's delivery cron polls `/watches` and routes
candidates -> quiet inbox and strong matches -> ping via the household/audience
resolver (see openclaw bead `openclaw-c0c4`). See
[`examples/watches.hdd.json`](examples/watches.hdd.json).

## Verify a deployment

```
kubectl -n <ns> port-forward svc/<release>-nagus 8080:8080
curl localhost:8080/healthz
curl -s -X POST localhost:8080/mcp -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
curl localhost:8080/watches
```
