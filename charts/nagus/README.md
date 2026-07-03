# charts/nagus

Helm chart for the **nagus** acquisition/watch subsystem. v1 deploys the **HDD
deal-watch vertical slice** as a single sqlite-backed pod and is structured to
expand to more categories and to the shared Postgres/CNPG backend without
restructuring.

## What it deploys

One `Deployment` running `nagus serve`:

- the **MCP server** at `/mcp` (JSON-RPC 2.0) -- the agent-facing surface, tools
  `search_items` + `get_item`, which openclaw registers for agents;
- a **plain-HTTP read-only pull** at `/search` + `/item` for debugging / non-MCP
  callers -- eyes, not hands;
- an optional **in-process ingest loop** (`serve.ingestInterval`) that collects
  eBay HDD listings, extracts typed fields, and scores $/TB into the store.

Plus a `Service`, a `ServiceAccount` (token automount off), and -- for the
sqlite backend -- a `PersistentVolumeClaim` mounted at `/data`.

sqlite is a single-writer single file, so the chart runs **one replica** with a
`Recreate` strategy: the same pod both ingests and serves, avoiding cross-pod
file sharing. Multi-writer (separate ingest CronJob + serve Deployment) is a
follow-on that arrives with the Postgres backend.

## Quick start (self-contained demo, no secrets, no network)

```
helm install nag charts/nagus --set demo.enabled=true
kubectl port-forward svc/nag-nagus 8080:8080
# MCP (what an agent sees):
curl -s -X POST http://127.0.0.1:8080/mcp -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_items","arguments":{"limit":5}}}'
# or the plain-HTTP debug pull:
curl 'http://127.0.0.1:8080/search?limit=5'
```

Demo mode mounts a bundled eBay fixture (`files/browse_search.json`) via a
ConfigMap and scores it offline, so `/search` returns ranked HDD $/TB deals
immediately.

## Real deployment

1. Provide eBay credentials as a Secret exposing `NAGUS_EBAY_CLIENT_ID` and
   `NAGUS_EBAY_CLIENT_SECRET`, one of:
   - `--set ebay.existingSecret=<name>` for a pre-made Secret, or
   - `--set externalSecret.enabled=true` plus `externalSecret.data[...]` to sync
     from Vault (never commit the values).
2. Enable ingest: `--set serve.ingestInterval=30m`.

Credentials are never placed in `values.yaml`; they live in Vault and reach the
pod via the Secret referenced above (design section 13).

## Key values

| key | default | meaning |
|---|---|---|
| `image.repository` / `image.tag` | `registry.orac.local/homelab/nagus` / appVersion | container image |
| `category` | `hdd` | category bundle (v1: hdd only) |
| `serve.ingestInterval` | `"0"` | in-process ingest cadence; `"0"` disables |
| `serve.minCapacityTB` | `6` | hard-filter capacity floor |
| `serve.offline` | `false` | score against the built-in demo reference |
| `demo.enabled` | `false` | self-contained fixture + offline scoring |
| `storage.backend` | `sqlite` | `sqlite` (wired) or `postgres` (placeholder, design 12.1) |
| `storage.sqlite.persistence.*` | 2Gi RWO | PVC for the sqlite file |
| `ebay.existingSecret` | `""` | Secret with the eBay OAuth credentials |
| `externalSecret.*` | disabled | sync eBay creds from Vault |

## Expansion path

- **More categories:** add a bundle in `internal/category`, then a values key to
  select it. The spine, store, and surface are unchanged.
- **Postgres backend:** `storage.backend: postgres` targets the shared CNPG
  cluster. The app store adapter for Postgres (nagus-nwc) and the chart wiring
  (nagus-quq) are the follow-on; provisioning is a gitops request per design
  section 12.1 (managed role + Database + ExternalSecret).
