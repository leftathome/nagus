# Shopify products.json connector-parser (per-store HDD sources)

Status: DESIGN (blocked on multi-connector ingest, nagus-dgq)
Beads: nagus-5n5 (this connector), nagus-dgq (prerequisite), context nagus-9nx
Date: 2026-07-26

## Why

The eBay Browse (Buy) API is the only eBay path with keyword search, and our
production keyset is not granted the Buy API production/client_credentials grant
(nagus-9nx). The App-ID-only alternatives are dead ends: the Finding API is
retired (returns a 503 HTML error page) and the Shopping API has no keyword
search (item lookup by id only). So HDD ingest needs a source that does not
depend on eBay.

Specialist HDD retailers (ServerPartDeals, PricePerGig, DatacenterDisk, ...) run
Shopify storefronts, and every Shopify store publishes a uniform, no-auth
`/products.json` feed. The per-store variation is pure configuration -- base
URL, store name, an optional product-type/tag allow-filter -- not code. So this
is ONE generic connector-parser configured per retailer, not a family of bespoke
connectors.

## Source identity: per-store

Each store is its own source. `SourceID() == "shopify:<name>"` (e.g.
`shopify:serverpartdeals`). This gives each store isolated freshness/purge
(`Store.DeleteStale` is scoped by `Connector.SourceID()`) and isolated failure:
one store 404ing or changing its catalog shape does not blow away another
store's rows.

## Hard prerequisite: multi-connector ingest (nagus-dgq)

Today `buildSourceConnector(cat, params)` returns exactly ONE
`listing.Connector`, and a serve process ingests from that single connector.
Per-store sources mean a single deployment runs MANY connectors (one per store),
each ingesting and purging independently. That multi-connector ingest loop is
exactly the refactor tracked by nagus-dgq (push source selection down, configure
a LIST of sources rather than one). This connector cannot ship until that lands;
nagus-5n5 depends on nagus-dgq.

## Shape

Package `internal/connector/shopify`. One `Connector` type, N configs.

```go
type Config struct {
    Name    string        // "serverpartdeals" -> SourceID "shopify:serverpartdeals"
    BaseURL string        // "https://serverpartdeals.com"
    MaxPages int          // pagination cap for ?limit=250&page=N
    Client  *http.Client
    // Allow-filter (optional): emit a variant only if the product matches.
    ProductType []string  // e.g. ["Hard Drives"]
    Tag         []string
    Vendor      []string
}

func (c *Connector) SourceID() string { return "shopify:" + c.Name }
```

### Fetch

Walk `GET {BaseURL}/products.json?limit=250&page=N`, N=1..MaxPages, stopping on
an empty `products` array. No auth.

Emit ONE `listing.Raw` per product VARIANT: a product "Seagate Exos" with
12/16/20 TB variants becomes three Raws, because each capacity is a distinct
sellable item the HDD extractor must see and score separately.

Apply the config allow-filter (product_type / tag / vendor) BEFORE emitting, so
a store that sells more than drives does not push its whole catalog downstream.
This is the operator decision of 2026-07-26 (config allow-filter over
"extractor rejects non-drives"): less downstream churn, at the cost of tuning a
filter per store.

### Raw mapping

| Raw field    | products.json source                     | Trust             |
|--------------|------------------------------------------|-------------------|
| SourceID     | `"shopify:" + Name`                      | --                |
| SourceKey    | `<product.id>:<variant.id>`              | trusted scalar    |
| SourceURL    | `BaseURL + "/products/" + handle`        | trusted           |
| Title        | `product.title`                          | UNTRUSTED         |
| Body         | strip-tags(`product.body_html`)          | UNTRUSTED (HTML stripped to text first; still crosses glovebox) |
| PriceCents   | `variant.price` * 100                    | trusted scalar    |
| Currency     | `"USD"` (config default; feed omits it)  | trusted           |
| ConditionRaw | `""` (specialist retailers = new)        | trusted           |
| Aspects      | product_type, vendor, variant options    | keys trusted, VALUES UNTRUSTED |
| SeenAt       | fetch time                               | --                |

Trust boundary is unchanged: Title/Body/Aspects-values are untrusted free text
and cross the glovebox Sanitizer before anything else, exactly as eBay Raw does.
`body_html` is stripped from HTML to text before it becomes `Body` -- we never
carry raw markup downstream -- but it remains untrusted.

### Downstream

Feeds the `hdd` category unchanged: reuses `category.NewHDDPipeline` and the
existing extractor. The connector is the only new surface. The extractor still
validates (min capacity, etc.), so the config allow-filter is an efficiency
gate, not the correctness gate.

## Test plan (when unblocked)

- Table-driven `Fetch` tests against a captured `products.json` fixture
  (httptest server): variant fan-out, pagination stop, allow-filter include/
  exclude, HTML-strip of body_html, price*100, SourceKey/SourceURL shape.
- SourceID stability test.
- Opt-in live integration test (build tag) against one real store, mirroring the
  eBay sandbox-integration pattern -- no money in the loop, read-only.
- Wire into the MemoryStore reference contract via the hdd pipeline end-to-end.

## Open items

- Currency: products.json omits currency; USD is a config default. Revisit if a
  non-US store is ever configured.
- Rate/politeness: set a descriptive User-Agent and a per-store fetch interval;
  these are storefront JSON endpoints but still someone's origin.
