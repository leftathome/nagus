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
