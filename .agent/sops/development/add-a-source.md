# SOP: add a data source

## 0. Check the source is legitimate FIRST

Before any code, establish that automated collection is permitted:

- Read `robots.txt` **and** the Terms of Use. A sanctioned API for *sellers*
  does not authorize buyer-side collection.
- Probe **without** `curl -L`. Following redirects reports the status of
  wherever you landed — a 301 chain ending in an HTML page looks like a 200 and
  will fool you into thinking a store serves JSON.
- Record what you found **with the date** in the design doc's source-viability
  table (§A.8.1). Sources change their stance; an undated "we looked once" is
  worthless.

Craigslist is prohibited. Newegg prohibits automated access outright.

## 1. Is it Shopify? Then it is config, not code

Any storefront serving `/products.json` is a config entry, no Go required:

```yaml
- name: <store>
  category: hdd          # OMIT for an offer-only speculative source
  type: shopify
  baseUrl: "https://<store>"
  intervalMinutes: 60    # politeness floor; these stores rate limit hard
  maxPages: 12           # watch for the TRUNCATED warning
  productTypePrefixes: ["Hard Drives", "HDDs"]   # if the catalogue is mixed
```

Include EVERY spelling a store uses for one category — serverpartdeals uses both
`Hard Drives >` and `HDDs >`.

## 2. Product-identity hints: declare, never infer

Only set `brandTag` / `skuIsMpn` / `skuSuffixes` when the store genuinely states
a manufacturer. If `vendor` is the store's own name, leave them unset — inferring
a brand there invents products.

## 3. No category? That is fine

A source with no category is offer-only: it accumulates offers with zero
evaluation cost. Use it for goods no category evaluates yet. It requires the
offer layer to be enabled, and refuses to start otherwise rather than silently
discarding everything.

## 4. Verify coverage, not just success

After deploying, check the log for `TRUNCATED`. A silent page cap makes partial
coverage look identical to full coverage — two live sources were cut short for
days before the warning existed.
