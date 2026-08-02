# nagus -> go-service-kit migration plan

**Status:** PLAN ONLY. No code has been changed. Nothing has been pushed.
**Date:** 2026-08-02
**Target:** `github.com/leftathome/go-service-kit` v0.1.2, go-service-template conventions
**Subject:** `nagus`, LIVE in production in the `nagus` namespace, Flux-managed.

> **For agentic workers:** this is a staged migration of a live service. Each stage
> is independently deployable and independently revertible. DO NOT batch stages.
> DO NOT proceed to stage N+1 until stage N's external verification has passed.
> Track progress by ticking the boxes; record the actual observed values in the
> "observed" lines, not just a tick.

---

## 0. Ground truth, verified today

Several items in the migration brief were stale. Verified against the working tree,
`git log`, and `steve/gitops` on 2026-08-02:

| Brief said | Actually true | Consequence |
|---|---|---|
| Two HelmReleases (`nagus` hdd, `nagus-land` land) | **ONE** HelmRelease `nagus`, merged 2026-07-29 (multi-source `sources[]` + `categories{}` in one config.json). `gitops/clusters/orac/apps/nagus/` contains only `helmrelease-nagus.yaml`. | Half the coordination cost the brief assumed does not exist. |
| ci-templates ref is v0.1.1 | ref is **v0.2.1** (`.gitlab-ci.yml:19`). v0.2.2 is what adds `EXTRA_BUILD_ARGS`. | Stage 1 is a one-minor bump, not a two-minor bump. |
| Single-arch build | **Already multi-arch** (nagus-viw fixed): two native kaniko legs + `.manifest-merge`, per-platform base digests in `Dockerfile`. Confirmed by the gitops comment recording an arm64 node running this exact tag. | Gap #10 is CLOSED. Do not touch it. |
| `NAGUS_WATCHES` ConfigMap has no checksum annotation | **Already fixed** in chart 0.5.1 / commit `fe0beeb` (nagus-bi5): `checksum/config`, `checksum/watches`, `checksum/demo` all present on the pod template. | Gap #6 is CLOSED. |
| `automountServiceAccountToken` not false | It **is** `false` on the ServiceAccount (`serviceaccount.yaml:12`). It is **not** set on the pod spec. | Gap #9 is half-closed; the belt-and-braces pod-level setting is still missing. |
| `version=dev` in production | **CONFIRMED.** `Dockerfile:34` declares `ARG VERSION=dev`; `.gitlab-ci.yml` build legs pass no `EXTRA_BUILD_ARGS`, so the deployed image is stamped `dev`. (The GitHub mirror `docker.yml:51` *does* pass it — but gitops consumes the GitLab registry image, tag `6a2ac8c5`.) | Gap #1 is real and Stage 1 fixes it. |
| Hand-rolled `/metrics` (eBay budget only) | CONFIRMED, `serve.go:89-120`. | Real. |
| `/readyz` hardcoded `"ready"` | CONFIRMED, `serve.go:73-76`. | Real. |
| No ServiceMonitor | CONFIRMED. Nothing anywhere scrapes nagus. | Real. |
| `http.DefaultClient` with no timeout | CONFIRMED, passed at `main.go:132,215` and `serve.go:267`; every connector also defaults to it internally. | Real. |
| `GOFLAGS: -mod=mod` | CONFIRMED, `.gitlab-ci.yml:32`. | Real. |
| Shutdown ~15s, default 30s grace | CONFIRMED, `serve.go:349`. `terminationGracePeriodSeconds` is unset in the chart, so it is Kubernetes' 30. | Real. |

### Who actually consumes nagus (exhaustive; searched openclaw, gitops, glovebox, quark, rom)

Three call sites, all in gitops, all `http://nagus.nagus.svc.cluster.local:8080`:

1. `gitops/clusters/orac/apps/glovebox/cronjob-openclaw-nagus-delivery.yaml:46`
   `NAGUS_WATCHES_URL: ".../8080/watches"` — CronJob every 30 min.
2. `gitops/clusters/orac/apps/glovebox/configmap-openclaw-nagus-delivery-script.yaml:19,26`
   Hardcoded default `.../8080/watches`, fetched with `curl -sf -m 20`, failure swallowed with `exit 0`.
   Consumes the exact JSON shape: `.watches[].{name,audience,strong_count,strong[]}`
   and per-row `.{id,verdict,score,title,capacity_tb,currency,price_cents,source_url}`.
3. `gitops/clusters/orac/apps/glovebox/configmap-openclaw-patches.yaml:410-423`
   openclaw gateway MCP client: `"url": ".../8080/mcp"`, `transport: streamable-http`,
   `timeout: 30`, `toolFilter.include: ["search_items","get_item"]`.

**Nothing external consumes `/search`, `/item`, `/metrics`, `/healthz`, or `/readyz`.**
No ServiceMonitor, no PrometheusRule, no Grafana dashboard, no PromQL anywhere
references `nagus_*`.

**Three load-bearing consequences:**

- **The port split is externally safe.** The kit's API port is 8080 — the same port
  `/watches` and `/mcp` already live on. The split only moves `/metrics`, `/healthz`
  and `/readyz` to 9090, and nothing outside the pod uses those. The only thing that
  must move in lockstep is the chart's own probe `port:`, which lives in the same
  chart. **gitops needs no change beyond version bumps.**
- **The JSON wire shape of `/watches` is a hard contract** with a shell script that
  does `jq` on named fields. Changing it is a bigger deal than changing the port.
  This is why huma is NOT adopted for the existing routes (see §4, Stage 7).
- **The consumer fails silently.** `curl -sf ... || exit 0`, and no alert watches the
  CronJob. A nagus breakage surfaces as "Telegram stopped pinging", days later.
  Every stage below therefore verifies from the *pod and Prometheus* side, never by
  waiting for a consumer to complain.

### Two operational facts that shape the whole plan

- **`strategy: Recreate`, `replicas: 1`.** Every deploy is a hard outage. A bad image
  is an outage, not a stalled rollout (nagus-viw). This is *not fixable* by moving to
  RollingUpdate — see §5, "Deliberately not doing".
- **`disableWait: true` on both `install` and `upgrade` in the HelmRelease.**
  Flux reports the HelmRelease `Ready` even when the pod is CrashLoopBackOff.
  **`flux get hr nagus` is not a verification step. Ever.** Every stage verifies the
  Pod and the endpoint, not the HelmRelease.

---

## 1. Baseline capture (Stage 0) — do this before anything else

Not a deploy. Everything below is a read.

- [ ] Record the currently deployed identity:
  ```bash
  kubectl -n nagus get deploy nagus -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'
  # observed: registry.orac.local/agentic/nagus/nagus:6a2ac8c5
  flux -n nagus get hr nagus -o wide     # chart version; NOT a health signal
  helm -n nagus get values nagus > /tmp/nagus-values-baseline.yaml
  helm -n nagus get manifest nagus > /tmp/nagus-manifest-baseline.yaml
  ```
- [ ] Capture the current wire shape of every endpoint, as golden files:
  ```bash
  kubectl -n nagus port-forward svc/nagus 18080:8080 &
  curl -s localhost:18080/watches            > /tmp/nagus-watches-baseline.json
  curl -s 'localhost:18080/search?limit=5'   > /tmp/nagus-search-baseline.json
  curl -s localhost:18080/metrics            > /tmp/nagus-metrics-baseline.txt
  curl -si localhost:18080/healthz           > /tmp/nagus-healthz-baseline.txt
  curl -si localhost:18080/readyz            > /tmp/nagus-readyz-baseline.txt
  curl -s -XPOST localhost:18080/mcp -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' > /tmp/nagus-tools-baseline.json
  curl -s -XPOST localhost:18080/mcp -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}' > /tmp/nagus-init-baseline.json
  ```
  These are the regression oracle for Stages 4, 5 and 7. `/tmp/nagus-watches-baseline.json`
  in particular is what the openclaw script parses.
- [ ] Record steady-state resource use, so Stage 4's memory growth is measurable:
  ```bash
  kubectl -n nagus top pod
  kubectl -n nagus get pod -o jsonpath='{.items[0].spec.containers[0].resources}{"\n"}'
  ```
- [ ] Record the current deploy outage duration, so Stage 5's regression is measurable:
  ```bash
  # in one shell
  while :; do curl -s -o /dev/null -w '%{http_code} ' -m 1 localhost:18080/healthz; sleep 0.2; done
  # in another
  kubectl -n nagus delete pod -l app.kubernetes.io/name=nagus
  # count the non-200s. observed: ____ seconds of outage
  ```
- [ ] Local kit spike, in a throwaway branch, **not committed**: `go get github.com/leftathome/go-service-kit@v0.1.2`,
  `go build ./...`, `ls -l` the binary. Record the binary size delta and the module count delta.
  Expected: ~15 MB -> ~40 MB, ~15 modules -> ~55 modules. This informs Stage 2's resource bump.

---

## 2. Stage table

| # | Stage | Go code? | Chart? | gitops? | Risk | Deploy is an outage? |
|---|---|---|---|---|---|---|
| 1 | CI/build hardening + real `VERSION` stamp | no | no | tag bump | **LOW** | yes (~5s, as today) |
| 2 | Chart hygiene: grace period, resources, pod token, revision history | no | yes | version bumps | **LOW** | yes (~5s) |
| 3 | ServiceMonitor + PrometheusRule, on the CURRENT port | no | yes | version bump | **LOW** | no pod restart |
| 4 | kit `obs`: JSON logs, real `/metrics`, still one listener | yes | resource only | version bumps | **MEDIUM** | yes (~5s) |
| 5 | kit `lifecycle` + real `/readyz` with a store `Ping`, still one listener | yes | grace/probe tuning | version bumps | **MEDIUM-HIGH** | yes (~10-15s) |
| 6 | Service-value metrics + the alert that actually matters | yes | alert rule | version bumps | **LOW** | yes (~10s) |
| 7 | kit `httpapi`: split API 8080 / admin 9090 | yes | ports, probes, SM | version bumps | **HIGH** | yes (~10-15s) |
| 8 | kit `outbound` for the connectors | yes | NetworkPolicy | version bumps | **MEDIUM** | yes (~10s) |
| 9 | kit `config` for the new knobs (gated on a kit fix) | yes | ConfigMap | version bumps | **MEDIUM** | yes (~10s) |

Stages 1-3 are pure infrastructure and can land back-to-back in a day.
Stage 7 is the dangerous one and should sit alone, on a day with time to watch it.

---

## 3. The stages

### Stage 1 — CI/build hardening. Fix `version=dev`. LOW RISK.

The lowest-risk deploy nagus will ever have: the *only* behavioural difference in
the binary is the string in `main.version`. Use it to prove the whole build->publish->
Flux->pod path still works before anything harder rides on it.

**Changes**

- `.gitlab-ci.yml`
  - `ci-templates` ref `v0.2.1` -> `v0.2.2`.
  - Add `SERVICE_VERSION` + `IMMUTABLE_TAG: "v${SERVICE_VERSION}-${CI_PIPELINE_IID}.${CI_COMMIT_SHORT_SHA}"`,
    matching the template. `SERVICE_VERSION` tracks `Chart.yaml` `appVersion`.
  - `EXTRA_BUILD_ARGS: "VERSION=$IMMUTABLE_TAG"` on **both** `build-amd64` and
    `build-arm64`. Setting it on one leg produces an index whose halves disagree
    about their own version — worse than neither.
  - `build` (`.manifest-merge`): add `MULTIARCH_SOURCE_TAG: "$CI_COMMIT_SHORT_SHA"` and
    per-rule `MULTIARCH_TAGS`. Today nagus leaves these unset and falls back to bare
    `$CI_COMMIT_SHORT_SHA` + `latest` — the lexically-sorting scheme that silently
    breaks Flux image automation. nagus pins tags manually today, so this is
    forward-looking, but the cost of doing it now is zero.
  - `GOFLAGS: "-mod=mod"` -> `"-mod=readonly"`.
  - New `lint` and `vulncheck` stages calling `make lint` / `make vulncheck`.
  - Pin the `golang:1.26` job image by digest to the SAME digest the Dockerfile's
    amd64 stage uses (`sha256:dbb10bd1...`), for the reasons in the template's CI
    header: a floating tag plus a warm zot cache means the toolchain gating your code
    is whatever got pulled first.
- New `Makefile`, copied from the template with `SERVICE=nagus`, `MODULE=github.com/leftathome/nagus`.
  Targets: `build test lint vulncheck tidy docker`. Both CIs call only these.
- `.golangci.yml`: adopt the template's `version: "2"` config
  (`errorlint bodyclose noctx copyloopvar gosec misspell revive` + `gofumpt goimports`).
  **Expect this to be red on first run.** Fix the findings on the branch; do not
  weaken the config. `bodyclose` and `noctx` in particular will flag the connectors,
  which is genuine.
- `.github/workflows/ci.yml`: call the same `make` targets so the two CIs cannot drift.
- Bump `Chart.yaml` `appVersion` to match `SERVICE_VERSION`.

**What could break in production**

- Nothing in the running process. The deployed change is one linker string.
- `-mod=readonly` may turn the pipeline red if `go.mod`/`go.sum` are out of sync.
  That is the point; run `make tidy` locally first and commit the result.
- The new lint config will be red. Budget for it; it is one branch, not one commit.
- The image tag *format* changes. Anything that pattern-matches nagus image tags
  would break — nothing does (verified: no ImagePolicy for nagus in gitops).
- One visible wire change: `initialize` -> `result.serverInfo.version` goes from
  `"dev"` to `"v0.5.2-NNN.abcd1234"`. openclaw's MCP client does not branch on it.

**External verification**

- [ ] Pipeline green on all six stages.
- [ ] Before deploying, prove the stamp locally:
      `crane export registry.orac.local/agentic/nagus/nagus:<newtag> - | tar -xO nagus > /tmp/nagus && /tmp/nagus version`
      Expect the immutable tag, **not** `dev`.
- [ ] Index still multi-arch (do not regress nagus-viw):
      `crane manifest registry.orac.local/agentic/nagus/nagus:<newtag> | jq '.manifests[].platform'`
      Expect `linux/amd64` and `linux/arm64`.
- [ ] Deploy: bump `image.tag` in `helmrelease-nagus.yaml`, `flux -n nagus reconcile hr nagus --with-source`.
- [ ] `kubectl -n nagus rollout status deploy/nagus --timeout=120s`
- [ ] `kubectl -n nagus get pod -w` until `1/1 Running`, restarts `0`.
- [ ] `curl -s -XPOST .../mcp -d '{"jsonrpc":"2.0","id":1,"method":"initialize"}' | jq -r .result.serverInfo.version`
      Expect the immutable tag.
- [ ] `diff <(curl -s .../watches) /tmp/nagus-watches-baseline.json` — structurally identical.

**Rollback**

Set `image.tag` back to `6a2ac8c5` in `helmrelease-nagus.yaml`, commit, `flux reconcile`.
~5s outage. The chart is untouched, so there is no chart/image pairing to worry about.

---

### Stage 2 — Chart hygiene, no Go changes. LOW RISK.

Everything here is a value the kit will *need* later, set now while the binary
still does not care. That is what makes Stage 5 and Stage 7 revertible: the chart
is already sized for the kit before the kit arrives.

**Changes** (all `charts/nagus/`)

- `deployment.yaml`
  - `terminationGracePeriodSeconds: {{ .Values.terminationGracePeriodSeconds }}`, default **60**.
    kit/lifecycle at its defaults needs 44s (propagation 4 + drain 20 + worker-stop 10 +
    flush 5 + margin 5); Kubernetes' default of 30 would SIGKILL mid-drain. 60 rather
    than the template's 45 because nagus is on longhorn and Recreate teardown is
    already the slow part. Setting it now is free: today's process exits in <15s
    regardless, so the pod terminates just as fast.
  - `automountServiceAccountToken: false` on the **pod** spec, in addition to the
    ServiceAccount. Two lines, no ambiguity, per the template.
  - `revisionHistoryLimit: {{ .Values.revisionHistoryLimit }}` (3) — so
    `kubectl rollout undo` has something to undo to.
  - `sizeLimit` on the `/tmp` emptyDir (64Mi). `readOnlyRootFilesystem: true` is
    already set, so `/tmp` is the only writable path and is currently unbounded.
  - `runAsNonRoot: true` / `runAsUser: 65532` / `seccompProfile` on the **container**
    `securityContext` too, not just the pod (nagus sets only the pod-level ones).
  - Add a `startupProbe` on `/healthz` (30 x 2s), so a slow postgres dial at start
    does not race the liveness probe.
- `values.yaml`
  - **Raise `resources.limits.memory` from 256Mi to 384Mi and requests from 64Mi to 128Mi.**
    This is the load-bearing change in this stage. Stage 4 adds the OTel SDK,
    `contrib/instrumentation/runtime`, the Prometheus client and (Stage 7) huma —
    roughly +40 modules and a 15MB -> 40MB binary. Under `Recreate`, an OOMKill loop
    is an outage, not a stalled rollout. Do the bump *before* the code that needs it,
    so if Stage 4 has to be reverted the memory headroom stays.
  - Add `terminationGracePeriodSeconds`, `revisionHistoryLimit`, `tmpSizeLimit`,
    `automountServiceAccountToken` keys.
- `Chart.yaml`: version bump, with the reasoning in the existing comment block style.

**What could break in production**

- A template typo -> Flux fails to apply -> **no change reaches the cluster**
  (`disableWait: true` means it will not stall the HR, but the render error appears
  in `flux get hr`). Safe failure.
- A bad `securityContext` -> the pod cannot start -> **outage under Recreate**. This
  is the one real risk. `runAsUser: 65532` matches distroless `:nonroot`, and the pod
  already runs with those values, so this is a duplication rather than a change — but
  render and diff before applying.
- Raising the memory *limit* cannot break anything; raising the *request* by 64Mi
  could in principle make the pod unschedulable on a full node. orac has headroom;
  check `kubectl describe node` if it Pends.

**External verification**

- [ ] `helm template charts/nagus -f /tmp/nagus-values-baseline.yaml > /tmp/new.yaml`
- [ ] `helm -n nagus get manifest nagus > /tmp/old.yaml; diff /tmp/old.yaml /tmp/new.yaml`
      — read every hunk. Nothing should change but the fields named above.
- [ ] `kubeconform -strict -summary /tmp/new.yaml`
- [ ] Deploy, then:
      `kubectl -n nagus get pod -o jsonpath='{.items[0].spec.terminationGracePeriodSeconds}'` -> `60`
      `kubectl -n nagus get pod -o jsonpath='{.items[0].spec.automountServiceAccountToken}'` -> `false`
      `kubectl -n nagus get pod -o jsonpath='{.items[0].spec.containers[0].resources.limits.memory}'` -> `384Mi`
- [ ] Confirm no token is mounted: `kubectl -n nagus get pod -o jsonpath='{.items[0].spec.volumes[*].name}'`
      should not contain a `kube-api-access-*` volume.
- [ ] `kubectl -n nagus top pod` — RSS unchanged from baseline.

**Rollback**

Revert the gitops chart-version bump. Chart-only, no image pairing. ~5s outage.

---

### Stage 3 — Make it observable BEFORE changing it. LOW RISK.

This is the stage that makes every later stage verifiable. nagus's `/metrics` has
never been scraped. Point a ServiceMonitor at the endpoint it has *today* — port
`http`, path `/metrics` — so there is a recorded baseline of `nagus_ebay_api_calls_*`
and of `up` to compare against after Stage 4 rewrites the endpoint and Stage 7 moves
it.

**Changes** (all `charts/nagus/`)

- New `templates/servicemonitor.yaml`, adapted from the template, but with
  `port: {{ .Values.serviceMonitor.port }}` defaulting to **`http`** (not `admin`).
  A comment must say, in as many words: *this points at the API port only until
  Stage 7 lands; it moves to `admin` in the same chart release that splits the
  listeners.* Getting that wrong later is failure mode #1 in §6.
- New `templates/prometheusrule.yaml`, the universal trio from the template
  (`NagusDown` on `up == 0` for 2m; `NagusHighMemory` at 0.85 of the limit;
  `NagusPodRestarting` > 2 in 30m). These are meaningful for the first time now
  that the memory limit is set (Stage 2) and a scrape target exists.
  Leave `alerts.extraRules` empty — the alert that actually matters needs a metric
  that does not exist yet, and arrives in Stage 6.
- New `templates/networkpolicy.yaml`, adapted from the template, **disabled by
  default** (`networkPolicy.enabled: false`). Write it now, ship it inert.
  Rationale: Flannel does not enforce policy on orac today, so this is a no-op —
  but the openclaw namespace already has a `default-deny-all` egress policy with
  **no rule permitting `openclaw -> nagus:8080`**. At the Cilium cutover both nagus
  consumers break for a reason unrelated to this migration. Writing nagus's side
  now, and filing a gitops bead for openclaw's egress rule, is cheap insurance.
- `values.yaml`: `serviceMonitor.{enabled: true, port: http, path: /metrics,
  interval: 30s, scrapeTimeout: 10s}`, `alerts.*`, `networkPolicy.enabled: false`.
  Default `serviceMonitor.enabled: true` so gitops needs no values change.

**What could break in production**

- No pod restart: `deployment.yaml` is untouched, so this is a pure add of two
  cluster-scoped-selected CRs. Verify by diffing the rendered manifest — if the
  Deployment hunk is non-empty, something is wrong.
- If `monitoring.coreos.com/v1` were absent, Flux would fail to apply. It is present
  (kube-prometheus-stack, `serviceMonitorSelectorNilUsesHelmValues: false`, so a
  chart-shipped ServiceMonitor is picked up from any namespace).
- Scraping `/metrics` on the API port adds ~2 requests/min of load to the same
  listener that serves `/mcp`. Negligible, but note that after Stage 7 those scrapes
  move off the API port, which is the correct end state.

**External verification**

- [ ] `diff` of rendered manifests shows **only** two new objects, no Deployment change.
- [ ] `kubectl -n nagus get servicemonitor,prometheusrule`
- [ ] Prometheus targets page (or API) shows `nagus/nagus` **UP**:
      `curl -s 'http://<prom>/api/v1/targets?state=active' | jq '.data.activeTargets[]|select(.labels.job=="nagus")|{health,lastError,scrapeUrl}'`
      Expect `health: "up"`, `scrapeUrl: "http://<podIP>:8080/metrics"`.
- [ ] `curl -s '<prom>/api/v1/query?query=nagus_ebay_api_calls_budget'` returns a
      sample with `source="ebay"`. **Record the value.** This is the metric that must
      survive Stage 4.
- [ ] `curl -s '<prom>/api/v1/query?query=up{job="nagus"}'` -> 1.
- [ ] `kubectl -n nagus get pod` — the pod's `AGE` is unchanged (no restart).

**Rollback**

`serviceMonitor.enabled: false`, `alerts.enabled: false`. No pod impact either way.

---

### Stage 4 — kit `obs`. Structured logs and a real `/metrics`, still ONE listener. MEDIUM RISK.

The key de-risking decision of this whole plan: **`obs` can be adopted without
`httpapi`.** `obs.Providers.PromRegistry` is a plain `*prometheus.Registry` and
`promhttp.HandlerFor` can be mounted on nagus's existing `http.ServeMux` at the
existing `/metrics` path on the existing port 8080. So this stage delivers runtime
metrics, build-info, and JSON logging with **zero change to the network surface**.

**Changes**

- `go.mod`: `require github.com/leftathome/go-service-kit v0.1.2`. Run `make tidy`.
  Expect ~40 new modules (otel, prometheus/client_golang, grpc, otlptranslator,
  x/time). `make vulncheck` must be green.
- `cmd/nagus/serve.go`
  - Call `obs.Setup(ctx, obs.Config{ServiceName: "nagus", ServiceVersion: version,
    ServiceInstanceID: os.Getenv("NAGUS_POD_NAME"), Environment: ..., LogLevel: ...})`
    early in `runServe`, before the store opens.
  - `slog.SetDefault(obsp.Logger)`.
  - Replace every `fmt.Fprintf(os.Stderr, ...)` in `serve.go` / `category.go` with
    structured `slog` calls. The `logf func(string, ...any)` seam threaded through
    `categoryOpts` into every connector stays — reimplement it as a thin adapter onto
    `obsp.Logger.Debug`, so internal packages keep their existing signature and this
    stays a `cmd/` change.
  - `defer obsp.Shutdown(context.WithoutCancel(ctx))` (belt and braces; Stage 5 moves
    the real flush into `lifecycle.Spec.Flush`).
  - Replace `handleMetrics` with `promhttp.HandlerFor(obsp.PromRegistry, promhttp.HandlerOpts{...})`
    mounted at the same `/metrics` on the same mux.
- **New `cmd/nagus/ebaymetrics.go`: preserve the eBay budget metric byte-for-byte.**
  This is non-negotiable — it is the only real operational signal nagus has today.
  Implement a `prometheus.Collector` (not an OTel instrument) and register it on
  `obsp.PromRegistry`:
  ```go
  // Names, HELP text and the `source` label MUST match serve.go's hand-rolled
  // output exactly. A native collector is used rather than an OTel gauge because
  // the OTel Prometheus exporter's UnderscoreEscapingWithSuffixes translation
  // renames instruments, and these names are already recorded in Prometheus.
  //
  // Collect() walks s.ingesters at SCRAPE TIME, so the UTC-midnight budget roll
  // and any X-RateLimit-Remaining cap are reflected without a background loop --
  // the same pull semantics the hand-rolled handler had.
  nagus_ebay_api_calls_budget{source="..."}
  nagus_ebay_api_calls_used{source="..."}
  nagus_ebay_api_calls_remaining{source="..."}
  ```
  A unit test must assert the exact family names and the label set. Use
  `testutil.CollectAndCompare` against a golden file derived from
  `/tmp/nagus-metrics-baseline.txt`.
- `charts/nagus`: add `NAGUS_POD_NAME` from the downward API (`metadata.name`) and
  `NAGUS_LOG_LEVEL` / `NAGUS_ENVIRONMENT` env. No port or probe change.

**Redaction: read this before wiring the logger.**

`obs.DefaultRedactKeys` is a fleet-wide deny list that is **actively wrong for nagus's
domain**. It redacts `query`, `search`, `address`, `lat`, `lon`, `title`. nagus's eBay
source config *is* a `query`; the land category *is* about `lat`/`lon`; a listing has
a `title`. Logging `slog.String("query", src.Query)` would emit `[redacted:a1b2c3d4]`
and confuse the next operator debugging a source.

Do **not** disable redaction (passing an empty non-nil slice turns it off entirely).
Instead build an explicit list at the call site:

```go
// Start from the kit's list, drop the keys that are operator-authored
// configuration in this service rather than user data, keep every credential key.
// A listing TITLE is untrusted third-party content and stays redacted at info
// level; log it only at debug, and never as an LLM instruction (CLAUDE.md).
RedactKeys: nagusRedactKeys()   // = DefaultRedactKeys minus {"query","search"}
```

Everything about untrusted listing content stays exactly as it is: extract still
emits the constrained typed schema, free text is still quoted as data, and nothing
about this stage widens what reaches an LLM. Do not log listing free text at info.

**What could break in production**

- **Memory.** Biggest risk in the stage, and why Stage 2 raised the limit first.
  Watch `container_memory_working_set_bytes` for an hour after deploy.
- **Metric name regression.** If `nagus_ebay_api_calls_*` changes name or label set,
  the only real signal nagus has is silently lost — Prometheus keeps the old series
  as stale and the new one starts from zero. The golden test plus the post-deploy
  PromQL check below are the guard.
- **Log format change.** `kubectl logs` goes from `nagus serve: source ebay fetched=3
  stored=3` to JSON. Anything (human or promtail pipeline) grepping the old format
  breaks. Nothing automated does today; the humans should be told.
- The `otel` SDK creates spans with no exporter configured (no OTEL_* set on orac, no
  Tempo). Per the kit docs that is silent and cheap — nothing dials. Verify no
  outbound connection attempts appear in the logs.
- `runtimeinst.Start` adds a background collection goroutine. Harmless, but it means
  the goroutine count baseline moves; do not read that as a leak.

**External verification**

- [ ] `kubectl -n nagus logs deploy/nagus --tail=20 | jq .` parses as JSON; every
      record has `service:"nagus"` and `version:"v0.5.x-NNN.sha"`.
- [ ] `curl -s .../metrics | grep '^nagus_ebay_api_calls'` matches the baseline
      family names and label sets exactly:
      `diff <(curl -s .../metrics | grep -E '^(# (HELP|TYPE) )?nagus_ebay' | sed 's/ [0-9.]*$//') \
            <(grep -E '^(# (HELP|TYPE) )?nagus_ebay' /tmp/nagus-metrics-baseline.txt | sed 's/ [0-9.]*$//')`
      **Must be empty.**
- [ ] New families present: `curl -s .../metrics | grep -cE '^(go_goroutine_count|go_memory_used_bytes|service_build_info|target_info)'` -> 4+.
- [ ] `service_build_info` carries the right version:
      `curl -s '<prom>/api/v1/query?query=service_build_info'` — check
      `version="v0.5.x-NNN.sha"` (NOT `dev`), `service="nagus"`.
      *Note the label collision the template documents: the ServiceMonitor's target
      `service` label wins, so nagus's own arrives as `exported_service`. Query on
      `job` + `namespace`.*
- [ ] `up{job="nagus"} == 1` still, and the scrape duration has not blown past
      `scrapeTimeout` (`scrape_duration_seconds{job="nagus"}` < 1).
- [ ] Memory after 1h: `container_memory_working_set_bytes{container="nagus"}`
      comfortably under 384Mi. **Record it.**
- [ ] `diff <(curl -s .../watches) /tmp/nagus-watches-baseline.json` structurally identical.
- [ ] MCP `tools/list` byte-identical to `/tmp/nagus-tools-baseline.json`.

**Rollback**

Pin the previous `image.tag`. Chart is unchanged except the two new env vars, which a
previous binary ignores — so **the chart and image are NOT paired in this stage** and
the image alone can be rolled back. That is deliberate: keep the pairing constraint
out of the medium-risk stages and confine it to Stage 7.

---

### Stage 5 — kit `lifecycle` + a real `/readyz`. Still ONE listener. MEDIUM-HIGH RISK.

**Changes**

- `internal/store/store.go`: add `Ping(ctx context.Context) error` to the `Store`
  interface. Implement in all three adapters:
  - `postgresstore`: `pool.Ping(ctx)` (already used at construction, `postgresstore.go:48`).
  - `sqlitestore`: `db.PingContext(ctx)`.
  - `store.MemoryStore`: `return ctx.Err()` — honours cancellation, never fails.
  Do the same for `offer.Store` if the offer layer is enabled, and register it as a
  second readiness check. **This is the only piece of `storekit` nagus adopts;
  see §5 for why the harness is not worth it.**
- `cmd/nagus/serve.go`
  - `ready := httpapi.NewReadiness(httpapi.WithCheckTimeout(3*time.Second))`
    `ready.Register("store", st.Ping)`; `ready.Register("offers", offerStore.Ping)` when on.
    Mount `ready.Handler()` at `/readyz` on the existing mux.
    (`httpapi.Readiness` is usable standalone — it does not require `NewAdmin`.)
  - Replace the whole `signal.NotifyContext` / `ListenAndServe` / `Shutdown(15s)`
    block with `lifecycle.Run(ctx, lifecycle.Spec{...})`, passing **one** server.
  - Convert the per-source ingest goroutines from bare `go runSourceIngestLoop(...)`
    into `lifecycle.Worker` entries, `FinishCurrentCycle: false` (abort stance — an
    ingest pass is idempotent and safely resumable; `Put` is an upsert).
    **`Worker.Run` must never return a non-nil error**, because the kit treats that as
    a process crash and nagus's whole design is per-source failure isolation. Keep the
    existing swallow-and-log behaviour inside the loop; return `nil` on `ctx.Done()`.
    (See §5, kit gap 7 — the kit has no "restart this worker with backoff" stance.)
  - `Flush: obsp.Shutdown` — runs LAST, after the drain, per the kit's sequence.

**The shutdown budget must be tuned for `Recreate`, not left at the kit defaults.**

The kit's `DefaultPropagationDelay = 4s` exists so a terminating pod keeps serving
while the endpoint controller withdraws it *and another replica takes the traffic*.
nagus has `replicas: 1` and `strategy: Recreate`: there is no other replica, the old
pod is gone before the new one starts, and every second of the sequence is **pure
added downtime with zero benefit**. Set:

```
PropagationDelay: 1 * time.Second   // not 4; there is nowhere else to route
DrainTimeout:     10 * time.Second  // nagus has one 30-min-cron consumer
FlushTimeout:     5 * time.Second
```

Grace period then = 5 (margin) + 1 + 10 + 10 (worker stop, budgeted even with abort
workers) + 5 = 31s, still under the 60 set in Stage 2. Keep 60; headroom is free.

Expected deploy outage: today ~5s, after this ~15s. **That is a regression and it is
the price of a correct drain.** Record it. If it proves painful, drop
`PropagationDelay` to `-1` (the kit disables the wait on a negative value), which is
defensible at `replicas: 1` behind `Recreate`.

**What could break in production**

- **`/readyz` can now return 503.** This is the single biggest behavioural change in
  the plan. A postgres hiccup that nagus previously rode out (hardcoded `ready`, and
  individual requests just 500'd) now removes the *only* pod from the Service
  endpoints and nagus goes completely dark. Mitigations, all required:
  - `failureThreshold: 3`, `periodSeconds: 10` on the readiness probe -> 30s of
    tolerance before eviction.
  - `WithCheckTimeout(3s)` so a slow-but-alive postgres is not called dead.
  - Liveness stays on `/healthz`, which consults nothing. **Never point liveness at
    `/readyz`** — a postgres outage would then CrashLoopBackOff the pod.
  - Stage 3's `NagusDown` alert now has something to catch. It only fires on `up == 0`
    (a failed *scrape*), and a not-ready pod is still scraped, so add a rule in Stage 6.
- Shutdown ordering bugs: if `Flush` runs before the drain, the last requests' telemetry
  is dropped on every deploy. `lifecycle.Run` gets this right; the risk is only if
  someone also calls `obsp.Shutdown` early. Keep the `defer` from Stage 4 (it is
  once-guarded, so the duplicate is free).
- `lifecycle.Run` binds the listener itself. Anything still calling `ListenAndServe`
  opts that listener out of the whole sequence. Grep for it after the change.

**External verification** — this stage's proof is watching the endpoint, not a curl.

- [ ] Readiness reports real checks:
      `curl -s .../readyz | jq .` -> `{"status":"ok","checks":[{"name":"offers",...},{"name":"store",...}]}`
      (Baseline was the literal string `ready`.)
- [ ] Prove it *fails* correctly, in a controlled way, before trusting it:
      scale the CNPG pooler / add a temporary NetworkPolicy blocking 5432 for 60s, then
      `curl -s -o /dev/null -w '%{http_code}' .../readyz` -> `503`, and
      `kubectl -n nagus get endpointslices -l kubernetes.io/service-name=nagus -o yaml | grep ready`
      -> `ready: false`. Then undo and confirm it returns to `true`. **Do this in a
      maintenance window.**
- [ ] Prove the drain order. In one shell:
      `kubectl -n nagus get endpointslices -l kubernetes.io/service-name=nagus -w`
      In another: `kubectl -n nagus delete pod -l app.kubernetes.io/name=nagus`
      Expect: `ready: false` appears **before** the pod's containers stop, and the
      logs show, in order:
      `lifecycle: readiness set to shutting down, still serving` ->
      `lifecycle: drain complete` -> `lifecycle: telemetry flushed` ->
      `lifecycle: shutdown complete`.
- [ ] No SIGKILL: the pod's last state is `Completed`/exit 0, not `Error`/137.
      `kubectl -n nagus get pod <old> -o jsonpath='{.status.containerStatuses[0].lastState}'`
- [ ] Measure the new outage duration with the Stage 0 loop. **Record it.**
- [ ] Ingest still runs: `kubectl -n nagus logs deploy/nagus | jq 'select(.msg|test("ingest"))'`
      shows a pass per source within one interval.
- [ ] `nagus_ebay_api_calls_used` still increments over an hour (proves the workers
      are actually running under lifecycle, not silently never started).

**Rollback**

Image tag only — the chart's grace period and probe settings from Stage 2/5 are
tolerated by the older binary (it will just answer `/readyz` with `ready` again).
Keep the readiness probe's `failureThreshold`/`periodSeconds` change in the chart;
it is harmless either way. **Chart and image are still not paired.**

---

### Stage 6 — The metric that would go flat. LOW RISK.

The template's house rule: *a service is not done until at least one metric would go
flat or red if it stopped doing its job, and that metric has an alert.* nagus can
serve 200s at p99 8ms while every source returns zero rows and the corpus goes stale.
Every panel in Stage 3's alert trio stays green through that.

**Changes**

- New OTel instruments on `obsp.Meter` (these are *new* names, so the OTel translator
  renaming them is fine — unlike the eBay budget, there is nothing to preserve):
  - `nagus_ingest_last_success_timestamp_seconds{source}` — gauge, set on every
    successful pass. **This is the one that matters.**
  - `nagus_ingest_items_stored_total{source}` — counter.
  - `nagus_ingest_errors_total{source,kind}` — counter, `kind` bounded to a small set
    (`budget_exhausted`, `fetch`, `extract`, `store`). Never put a listing value in a
    label.
  - `nagus_store_items{category}` — gauge, observed at scrape time.
  - `nagus_watch_strong_matches{watch}` — gauge, so "the pings stopped" is visible.
- `charts/nagus/values.yaml`, `alerts.extraRules`:
  ```yaml
  - alert: NagusIngestStalled
    expr: time() - max by (source) (nagus_ingest_last_success_timestamp_seconds) > 10800
    for: 15m
  - alert: NagusNotReady          # the gap Stage 5 opened
    expr: kube_pod_status_ready{namespace="nagus",condition="true"} == 0
    for: 10m
  - alert: NagusEbayBudgetExhausted
    expr: nagus_ebay_api_calls_remaining == 0
    for: 30m
  ```
  The staleness threshold is 3h because the slowest configured source
  (`savemyserver`) polls every 360 min — set it above the slowest interval or it
  flaps.
- Optional: a `dashboards/configmap-grafana-dashboard-nagus.yaml` applied via gitops
  to the `monitoring` namespace (Grafana's sidecar `searchNamespace` is not overridden
  on orac, so an app-namespace dashboard ConfigMap is silently ignored).

**What could break** — essentially nothing in the data path. The risk is metric
cardinality: `source` is bounded by config (4 today), `kind` by the enum, `watch` by
config (1 today). Do not add an `item_id` or `url` label, ever.

**External verification**

- [ ] `curl -s '<prom>/api/v1/query?query=nagus_ingest_last_success_timestamp_seconds'`
      returns one series per configured source with a recent timestamp.
- [ ] Force a stall: temporarily set one source's `intervalMinutes` absurdly high (or
      block its egress), confirm `NagusIngestStalled` moves to `pending` then `firing`
      in `<prom>/alerts`, then undo.
- [ ] `kubectl -n nagus get prometheusrule nagus -o yaml | grep -c alert:` -> 6.

**Rollback** — image tag; `alerts.extraRules: []`.

---

### Stage 7 — kit `httpapi`: split API 8080 / admin 9090. HIGH RISK. Do this alone.

**Design decision, made deliberately: adopt the kit's *listeners*, not huma.**

`/search`, `/item`, `/watches` and `/mcp` keep their existing handlers, registered on
`api.Mux`. They are NOT modelled as huma operations. Reasons:

- `/watches`'s JSON shape is parsed field-by-field by a shell script in gitops.
  Re-modelling it through huma risks a silent shape change (`omitempty` behaviour,
  null-vs-`[]` for empty slices, field ordering).
- The kit's house error shape is RFC 9457 problem+json. `/mcp` **must not** return
  problem+json — it must return a JSON-RPC 2.0 error object. Two error formats on one
  listener is exactly what the kit says not to do, and nagus has no choice.
  See §5, kit gap 1.
- The kit's house list envelope (`{items,next_cursor,has_more}`) and its
  `limit` cap of 200 are both breaking changes to `search_items`.

What nagus *does* get from `httpapi`: the hardened `http.Server` (mandatory timeouts,
`ReadHeaderTimeout`), `otelhttp` RED metrics with a bounded label set including
`http_route`, and the admin/API split with `/metrics` and pprof off the public port.
That is worth having on its own.

**Changes**

- `cmd/nagus/serve.go`
  ```go
  api := httpapi.New(httpapi.Options{
      Addr: cfg.Listen,                   // ":8080", unchanged
      Title: "nagus", Version: version,
      DocsEnabled: false,                 // no huma operations to document
      TracerProvider: obsp.TracerProvider,
      MeterProvider:  obsp.MeterProvider,
      Logger:         obsp.Logger,
  })
  // Existing handlers, unchanged, on the raw mux. Method-qualified patterns so a
  // POST to /search is a 405 from the mux rather than reaching the handler.
  api.Mux.HandleFunc("GET /search",  srv.handleSearch)
  api.Mux.HandleFunc("GET /item",    srv.handleItem)
  api.Mux.HandleFunc("GET /watches", srv.handleWatches)
  api.Mux.HandleFunc("POST /mcp",    srv.handleMCP)

  admin := httpapi.NewAdmin(httpapi.AdminOptions{
      Addr: cfg.AdminListen,              // ":9090"
      Readiness: ready, Registry: obsp.PromRegistry,
      PprofEnabled: cfg.PprofEnabled, Logger: obsp.Logger,
  })
  lifecycle.Run(ctx, lifecycle.Spec{Servers: []*http.Server{api.Server, admin.Server}, ...})
  ```
- Delete the hand-rolled `/healthz`, `/readyz`, `/metrics` routes from `routes()`;
  `NewAdmin` provides all three on 9090.
- **`WriteTimeout` audit — mandatory before deploying.** `httpapi` sets a
  non-configurable `WriteTimeout: 30s`. nagus's server has **no** write timeout today,
  and `/watches` evaluates every watch (a store `Search` plus per-item valuation,
  which may make outbound calls) using `http.DefaultClient`, which also has no
  timeout. A `/watches` call that takes >30s is currently slow-but-correct; after
  this stage it is truncated mid-body. Measure it first:
  `for i in $(seq 20); do curl -s -o /dev/null -w '%{time_total}\n' .../watches; done`
  If p99 is anywhere near 30s, **do Stage 8 first** (the `outbound` client bounds the
  valuation calls) or bound the handler with `context.WithTimeout` explicitly.
- `charts/nagus/`
  - `deployment.yaml`: second container port `admin: 9090`; **move `livenessProbe`,
    `readinessProbe` and `startupProbe` from `port: http` to `port: admin`**.
  - `service.yaml`: second Service port `admin: 9090` -> `targetPort: admin`.
    (Without it the ServiceMonitor selects nothing — a port with no Service endpoint
    is invisible to the operator, silently.)
  - `servicemonitor.yaml`: `serviceMonitor.port: http` -> **`admin`**.
  - `values.yaml`: `service.apiPort: 8080`, `service.adminPort: 9090`,
    `serviceMonitor.port: admin`, `config.pprofEnabled: false`.
  - `networkpolicy.yaml`: monitoring ingress rule now targets `adminPort`.

**What could break in production — the worst stage, enumerated**

1. **Chart/image version skew.** Chart >=0.8 probes 9090; images <0.8 do not listen
   there. Liveness fails -> container killed -> CrashLoopBackOff -> and under
   `Recreate` the old pod is already gone. Flux reports the HelmRelease `Ready` the
   whole time because `disableWait: true`. **This is failure mode #1 and it is the
   single most likely way this migration causes an incident.**
   **Mitigation, mandatory:** chart version and image tag are bumped in ONE gitops
   commit and are treated as an atomic pair forever after. Write the pairing into the
   `helmrelease-nagus.yaml` comment block (that file already documents pairings this
   way). **Rolling back one without the other is the incident.**
2. `/metrics` disappears from 8080 the instant the new pod starts. Nothing external
   scrapes it, and the ServiceMonitor moves in the same chart — but if the
   ServiceMonitor's `port` is left at `http`, the operator generates a scrape config
   matching no endpoint and the target list goes *empty*. Not "down" — **empty**, so
   `up{job="nagus"}` produces no series and `NagusDown` (`up == 0`) never fires.
   Check the targets page explicitly, do not trust the absence of an alert.
3. Handlers registered on `api.Mux` are wrapped by `otelhttp`, so every `/mcp` POST
   now emits RED series. `http_route` will be `/mcp`, `/search`, etc. — bounded. Good.
   But the Prometheus scrape of `/metrics` is now on the un-instrumented admin
   listener, which is the correct end state and means the RED baseline shifts.
4. Method-qualified mux patterns change 405 behaviour: today `handleSearch` returns
   `405 method not allowed` as *plain text* from its own check; with
   `"GET /search"` the mux returns the stdlib 405. Body differs. Nothing parses it.
5. `httpapi.New` sets `ReadTimeout: 30s`, `IdleTimeout: 120s`, `MaxHeaderBytes: 1MB`.
   The openclaw MCP client uses `timeout: 30` — exactly at the boundary. Watch for
   truncated MCP responses.

**External verification**

- [ ] Render and diff the chart **first**; confirm exactly: one new container port,
      one new Service port, three probes moved, one ServiceMonitor port changed.
- [ ] Deploy chart+image together, then, from inside the namespace
      (`kubectl -n nagus run tmp --rm -it --image=curlimages/curl --restart=Never -- sh`):
      - `curl -s http://nagus:8080/watches | jq '.watches|length'` -> unchanged
      - `curl -s -o /dev/null -w '%{http_code}' http://nagus:8080/metrics` -> **404**
      - `curl -s -o /dev/null -w '%{http_code}' http://nagus:8080/healthz` -> **404**
      - `curl -s http://nagus:9090/healthz` -> `ok`
      - `curl -s http://nagus:9090/readyz | jq .status` -> `ok`
      - `curl -s http://nagus:9090/metrics | grep -c nagus_ebay` -> >0
      - `curl -s -o /dev/null -w '%{http_code}' http://nagus:9090/debug/pprof/` -> **404** (pprof off)
      - `curl -s -o /dev/null -w '%{http_code}' http://nagus:9090/search` -> **404** (no leakage)
- [ ] `diff <(curl -s http://nagus:8080/watches) /tmp/nagus-watches-baseline.json` structural match.
- [ ] MCP end to end through the real consumer:
      `curl -s -XPOST http://nagus:8080/mcp -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'`
      byte-identical to `/tmp/nagus-tools-baseline.json`.
- [ ] Probes: `kubectl -n nagus describe pod | grep -A2 -E 'Liveness|Readiness|Startup'`
      all show `:9090`. Restart count `0` after 10 minutes.
- [ ] Prometheus target `scrapeUrl` is now `http://<podIP>:9090/metrics` and `health: up`.
      **Explicitly confirm the target still EXISTS** — an empty target list is the
      silent failure here.
- [ ] New RED families present: `http_server_request_duration_seconds_count{http_route="/mcp"}`.
- [ ] Run the openclaw delivery CronJob manually and confirm it still pings:
      `kubectl -n openclaw create job --from=cronjob/openclaw-nagus-delivery verify-$(date +%s)`
      then read its logs. This is the only end-to-end proof of the real consumer.

**Rollback**

Revert the gitops commit that bumped **both** chart version and image tag. One commit,
one revert, `flux reconcile`. ~15s outage. **Do not use `helm rollback` here** — it
would revert the chart while Flux keeps reasserting the new image tag from git.

---

### Stage 8 — kit `outbound` for the connectors. MEDIUM RISK.

Currently `http.DefaultClient` (no timeout) is handed to every connector and enricher.
A stalled remote parks an ingest goroutine indefinitely, and after Stage 7 a slow
valuation can hit the 30s `WriteTimeout` on `/watches`.

**Changes**

- Widen the connectors' `HTTPClient *http.Client` field to a small interface
  `interface{ Do(*http.Request) (*http.Response, error) }` in `ebay`, `shopify`,
  `zillapi`, `enrich/geo`, `enrich/parcel`, `valuation/hdd`. `*http.Client` and
  `*outbound.Client` both satisfy it. **This is 6 files of mechanical change and it
  exists only because the kit does not offer an `*http.Client`-shaped adapter — see
  §5, kit gap 2.**
- One `outbound.Client` per integration, each with the terms comment the kit's package
  doc mandates:
  - eBay Browse: `Timeout 20s, RequestsPerSecond 2, Burst 2` — with a comment
    recording License 2.4 (~5k/day) and 8.1(b) (6h retention). **Do NOT use
    `outbound.Config.CallBudget`**: it is a process-lifetime counter that never
    resets, whereas nagus's budget is per-UTC-day and can be capped by eBay's
    `X-RateLimit-Remaining` header. Keep `internal/connector/ebay/budget.go`. (kit gap 3.)
  - Shopify storefronts: `Timeout 20s, RequestsPerSecond 0.5, Burst 1`, with the
    "serverpartdeals returns 429 local_rate_limited" note. `Retry-After` handling is a
    genuine improvement here.
  - Zillapi: `Timeout 30s, RequestsPerSecond 1` + the one-credit-per-result comment.
  - Rentcast / geo: `Timeout 15s`.
- `ContactURL` must be a real reachable URL, per `outbound.New`'s validation.
- **`AllowPrivateNetworks: false` on all of them** (the default). nagus fetches
  attacker-influenced URLs; the SSRF guard is exactly right. Note: this also means
  `Proxy` is disabled on those transports.
- `charts/nagus/networkpolicy.yaml`: enumerate the egress destinations. Still inert
  under Flannel, but it must be right before Cilium.

**What could break**

- Rate limiting is now *enforced* client-side. If `RequestsPerSecond` is set too low,
  a source's poll takes longer and may not finish inside an interval. Watch
  `nagus_ingest_last_success_timestamp_seconds` per source after deploy.
- The SSRF guard refuses private/loopback/CGNAT destinations. If any fixture path or
  a future internal source points at a cluster-local URL, it will be refused. The
  three live sources are all public internet.
- `outbound` sets its own `User-Agent`, replacing whatever the connectors send. A
  storefront that allow-lists by UA would start 403ing. Verify with one manual poll.
- Retries on 429/503 mean the eBay call budget is consumed per *attempt*. `callBudget.reserve()`
  is called by the connector per logical call, so the two counters can drift. Decide
  explicitly: wire `outbound.Config.Metrics` to also feed `callBudget.reserve()`, or
  set `MaxAttempts: 1` for the eBay client. **Recommend `MaxAttempts: 1` for eBay**
  — a budgeted API is the wrong place for silent retries.

**External verification**

- [ ] `nagus_ingest_last_success_timestamp_seconds` fresh for all four sources after
      one full cycle of the slowest (6h).
- [ ] `nagus_ingest_errors_total{kind="fetch"}` does not step up after deploy.
- [ ] `nagus_ebay_api_calls_used` grows at the same rate as before (no retry inflation).
- [ ] No `outbound: destination address is not permitted` in the logs.
- [ ] Confirm the UA on the wire: run one source against a local `nc -l` in a debug
      pod, or check a storefront's response codes for 24h.

**Rollback** — image tag only.

---

### Stage 9 — kit `config`, for the new knobs only. MEDIUM RISK. **BLOCKED on a kit fix.**

`config.Load[T]` supports `string, int, int64, bool, time.Duration, []string`.
**It does not support `float64`.** nagus has three float knobs — `NAGUS_MIN_CAPACITY`,
`NAGUS_LAND_MIN_ACREAGE`, `NAGUS_LAND_MAX_ACREAGE` — so a straight port is impossible
today. See §5, kit gap 4.

Recommendation: **do not migrate nagus's existing NAGUS_* configuration at all in this
effort.** Renaming or re-parsing 20 env vars on a live service is all risk and no
reward: Helm renders an unrecognised key into nothing, so a typo shows up as
"the setting had no effect", not as an error — the exact failure mode nagus already got
bitten by twice (nagus-bi5).

Stages 4-8 introduce their new knobs (`NAGUS_ADMIN_LISTEN`, `NAGUS_LOG_LEVEL`,
`NAGUS_ENVIRONMENT`, `NAGUS_PPROF_ENABLED`, `NAGUS_SHUTDOWN_*`) with the existing
`envOr`/`envDuration` helpers. When the kit grows `float64`, a follow-on stage can
introduce `internal/config` using `kitconfig.Load[T]` for **everything at once**,
keeping the `NAGUS_` prefix (not `SVC_`), with a startup log line dumping the resolved
non-secret config so an operator can diff intent against reality.

Bead this; do not do it inside the migration.

---

## 4. Deliberately NOT migrating

| Thing | Why not |
|---|---|
| **huma for `/search`, `/item`, `/watches`** | `/watches`'s JSON is parsed field-by-field by a gitops shell script. The kit's `Page[T]` envelope and problem+json errors are both breaking changes to a live consumer. Register the existing handlers on `api.Mux` instead. Revisit if a *new* v2 API is ever added alongside. |
| **`/mcp` through huma** | MCP is a single POST whose behaviour is dispatched by a `method` field in a polymorphic body. OpenAPI cannot describe it usefully, and JSON-RPC errors are not problem+json. It stays a raw `api.Mux` route. |
| **`httpapi.PageParams` / cursor pagination** | Would change `search_items`'s response shape and cap `limit` at 200. openclaw's tool filter depends on the current shape. |
| **`httpapi` docs UI / OpenAPI** | Nothing is modelled in huma, so there is nothing to document. `DocsEnabled: false`. |
| **`storekit.Harness`** | nagus's `Store` is `Put/Get/Search(Query)/DeleteStale` — it has no `List`, no cursor, and no single-key `Delete`. Satisfying the harness means adding three methods to three adapters purely for a test, plus inventing an opaque cursor. Worse, the harness requires a **total, stable** `List` order; nagus's `Search` orders by `SeenAt DESC`, which is not total, so satisfying it would force a tiebreaker change to the live query. The harness's *clauses* are already covered by nagus's own `MemoryStore` contract tests, which are the documented reference in CLAUDE.md. **Adopt `Ping` only** (Stage 5) — that is the one clause that buys something real. |
| **`outbound.Config.CallBudget`** | Process-lifetime, non-resettable, and cannot ingest a server-reported remaining count. nagus's budget is per-UTC-day with `X-RateLimit-Remaining` capping. Keep `internal/connector/ebay/budget.go`. |
| **`SVC_*` env prefix** | Renaming live env vars is a gratuitous outage risk with zero benefit. Keep `NAGUS_*`. |
| **Moving to `RollingUpdate`** | Tempting, since `storage.backend: postgres` makes the RWO PVC vestigial. **But nagus runs per-source ingest loops that poll immediately on start.** A rolling update means two pods polling every source simultaneously: a double-poll of `serverpartdeals`, which rate-limits hard, and double consumption of the eBay daily budget. The template's own values.yaml states the invariant: *any enabled worker loop means exactly one replica*. Keep `Recreate`. Consequence: tune `PropagationDelay` DOWN (Stage 5), because under Recreate it is pure downtime. |
| **Deleting the legacy sqlite PVC** | It still holds the pre-cutover corpus and the chart's PVC template has **no** `helm.sh/resource-policy: keep` — a `helm uninstall` or a values flip would delete it irrecoverably. Adding the annotation is a good separate one-line change; deleting the volume is a separate decision with its own bead. |
| **kit `config` for existing knobs** | Blocked on `float64`; see Stage 9. |

---

## 5. Kit shortcomings found by the first real consumer

This is the section that outlives the migration. quark and rom will hit all of these.

**1. There is no story for a non-REST surface on the public listener.**
The kit is heavily huma-shaped: `httpapi.Options` is about OpenAPI, `errors.go` is
about problem+json, `paging.go` is about a house envelope. nagus's primary consumer-
facing endpoint is JSON-RPC 2.0 over a single POST. It can only live on `API.Mux`,
where it gets `otelhttp` (good) but is invisible to the OpenAPI document and **must**
violate the package's stated one-error-format-per-listener invariant, because a
JSON-RPC error is not problem+json. The kit should either (a) document the "raw route
on `API.Mux`" pattern as first-class, with guidance on the error-shape exception, or
(b) grow an `httpapi.RawRoute` affordance that makes the trade explicit. Any service
exposing MCP — which in this fleet means most of them — is in this position.

**2. `outbound.Client` is not adoptable in O(1).**
`*outbound.Client` is not an `*http.Client`, does not implement `http.RoundTripper`,
and the package exports no interface a service can type its fields as. Every one of
nagus's six integrations declares `HTTPClient *http.Client`, so adopting the kit means
touching all six. **Fix: export `type Doer interface { Do(*http.Request) (*http.Response, error) }`**
in `outbound` so services can write `HTTPClient outbound.Doer` from day one, and/or
provide `(*Client).RoundTripper() http.RoundTripper` so a service can keep
`*http.Client` and get the guard, rate limiter and retries via `Transport`. The second
option would make adoption a one-line change per integration.

**3. `outbound`'s call budget is the wrong shape for a real quota.**
`CallBudget` is a process-lifetime `atomic.Int64` that never resets. Real API quotas
are windowed (eBay: per UTC day) and are often reported by the server
(`X-RateLimit-Remaining`). The kit has no window, no reset, no way to feed a
server-reported remaining count, and no `BudgetRemaining()` accessor (only
`CallsUsed()`; the remaining count is only observable as a side effect on `CallEvent`).
nagus therefore cannot retire its own `callBudget`, which is exactly the code the kit
cites in its own package doc as the precedent. Fix: a `Budget` interface with a
`Window time.Duration`, a `Reset()`, and an `Observe(remaining int64)` hook.

**4. `config.Load[T]` has no `float64`.**
Hard blocker for nagus (`NAGUS_MIN_CAPACITY`, `NAGUS_LAND_{MIN,MAX}_ACREAGE`).
Any service with a threshold, a ratio, or a unit measurement will hit this. `float64`
and `[]int` are the obvious additions. Also worth considering: `map[string]string`
(for label sets) and a documented "read this from a file, not the env" escape.

**5. There is no `obs` -> `outbound` bridge.**
`outbound.Metrics` is described as "the seam the observability package wires into",
but `obs` provides no implementation. Every service will hand-roll an adapter from
`CallEvent` to instruments, and every service will pick different metric names —
precisely the divergence the kit exists to prevent. **Fix: ship
`obs.OutboundMetrics(meter metric.Meter) outbound.Metrics`** with fixed names
(`outbound_calls_total{host,method,status}`, `outbound_call_duration_seconds`,
`outbound_limiter_delay_seconds`, `outbound_budget_remaining{host}`).

**6. `httpapi` has no single-port or transition mode.**
For a green-field service the two-listener split is right. For a live service with
existing probes, the migration is a hard cutover with no "serve both for one release"
option. A migrating service can hand-mount `ready.Handler()` and
`promhttp.HandlerFor(reg, …)` on `api.Mux` — but that reimplements `NewAdmin`, loses
its method-qualified route strictness, and puts `/metrics` behind `otelhttp`, which is
exactly the self-referential-scrape-traffic problem `admin.go`'s comment says the
split avoids. **Fix: export `httpapi.AdminHandlers(AdminOptions) map[string]http.Handler`**
(or `Admin.RegisterOn(*http.ServeMux)`) so a service can serve the admin paths on both
ports for one release, plus an `otelhttp.WithFilter` recipe to exclude them from RED
metrics while they are on the public mux.

**7. `lifecycle.Worker` has only two stances, and neither fits a multi-source poller.**
`FinishCurrentCycle: false` = abort silently; returning an error = crash the whole
process. nagus's design is per-source failure isolation: one source's failure must not
touch another source or the read surface. So nagus's workers must never return an
error, which means the kit's "a worker whose loop has died while the service keeps
reporting ready is worse than a restart" guarantee is inert for nagus. **Fix: a third
stance — restart the worker with backoff, and expose the restart count as a metric.**
"One of my four pollers died" should be an alert, not a process crash and not silence.
Any service with N independent background loops needs this.

**8. `lifecycle`'s propagation delay is wrong under `strategy: Recreate`.**
`DefaultPropagationDelay = 4s` assumes another replica picks up the traffic while this
one is withdrawn. At `replicas: 1` with `Recreate` there is no other replica, the old
pod is gone before the new one starts, and those 4 seconds are **pure added downtime
with zero benefit**. The package doc explains the delay thoroughly but never names
this interaction. **Fix: one paragraph on `Spec.PropagationDelay` saying so, and a
recommendation to set it near zero (or negative) for single-replica Recreate
deployments.** The template's values.yaml already flags Recreate as a hazard; the two
documents should agree.

**9. `httpapi`'s `WriteTimeout: 30s` is unconfigurable, and 30s is not always enough
for a slow READ.** The package doc anticipates "streams responses or accepts large
uploads" as the exception. nagus's `/watches` is neither: it is a fan-out read that
evaluates every watch, each doing a store query plus per-item valuation enrichment
that may make outbound calls. It has no write timeout today. Silent mid-body
truncation is a nasty failure mode for a JSON consumer. **Fix: either make it an
`Options` field with a documented ceiling, or add explicit guidance that any handler
which can exceed the budget must carry its own `context.WithTimeout` and return a
`504` rather than being cut.**

**10. `obs.DefaultRedactKeys` is a fleet policy that is wrong for some domains.**
It redacts `query`, `search`, `address`, `lat`, `lon`. nagus is an acquisition service:
`query` is operator-authored source configuration, and `lat`/`lon` are the land
category's whole subject. The doc says "extend it, do not replace it", and the only
alternatives are replacing it wholesale or disabling redaction (an empty non-nil
slice) — both of which risk dropping a credential key. **Fix: `obs.RedactKeysExcept(...)`,
or split the list into `CredentialRedactKeys` (never remove) and
`ContentRedactKeys` (service-tunable).**

**11. `storekit.Store` is a shape, not a contract, despite the doc's claim.**
The package doc says the kit deliberately avoids a universal Store because "nagus has
its own" — and then defines a five-method interface that nagus's store does not and
should not implement. nagus has `Search(Query)` (richer than `List`) and
`DeleteStale(source, before)` (bulk, scoped) instead of `List(Cursor)` and
`Delete(key)`. The harness is also all-or-nothing: `Harness.Run` runs ten subtests and
there is no way to run the six that apply. **Two fixes: (a) export
`type Pinger interface{ Ping(context.Context) error }` separately, since readiness is
the clause with the highest value-to-cost ratio and it should not require the whole
shape; (b) make the clauses individually runnable (`Harness.RunClauses("Ping",
"PutGetRoundTrip", …)`) so a partially-conforming adapter can still be gated on what
it does promise.**

**12. Preserving an existing metric name requires bypassing OTel, and this is
undocumented.** `obs` exposes `PromRegistry`, which is the correct escape hatch — but
nothing says *when to use it*. A service migrating an existing `/metrics` cannot
re-express its metrics as OTel instruments without the
`UnderscoreEscapingWithSuffixes` translator renaming them (unit suffixes, dot-to-
underscore), silently orphaning every recorded series. **Fix: a paragraph in `obs`
saying "to preserve an existing metric family name across a migration, register a
native `prometheus.Collector` on `Providers.PromRegistry`; OTel instruments are
renamed by the translator."** This is exactly what nagus must do for
`nagus_ebay_api_calls_*`, and it took reading `newMeterProvider` to work out.

**13. Dependency weight.** Adopting the kit takes nagus from 15 modules to ~55 and the
static binary from ~15MB to ~40MB, on a cluster whose registry pulls come over a
residential uplink (`nagus-c4p`). That is the right trade for real observability, but
the kit's README should state it, because a service with a 256Mi limit and a
`Recreate` strategy — as nagus had — turns it into an OOMKill outage rather than a
line item.

**14. Minor:** `obs.Setup` calls `runtimeinst.Start` with no guard against being
called twice in one process; `httpapi.Options` has no `BaseContext`; the kit's
`go.mod` `go 1.26.0` directive is correct post-0.1.1 but the CHANGELOG entry is worth
mirroring into the template's docs so nobody re-introduces a patch-level directive.

---

## 6. The three things most likely to cause a production incident

**1. Chart/image version skew across the Stage 7 port split.**
Chart >=0.8 points all three probes at `:9090`. Any image built before Stage 7 does not
listen there. The liveness probe fails, the kubelet kills the container, and under
`strategy: Recreate` the old pod is already gone — so this is a full outage, not a
stalled rollout. `disableWait: true` means Flux reports the HelmRelease `Ready`
throughout, and nothing alerts, because the only nagus consumer swallows its own
failures (`curl -sf … || exit 0`) and there is no alert on the CronJob.
The likeliest trigger is not the deploy — it is the **rollback**: reverting only the
image tag "to get back to a known-good binary" while leaving the new chart in place.
*Mitigations:* bump chart and image in one gitops commit; record the pairing in the
HelmRelease comment block; roll back by reverting that commit, never by editing one
field; and add `NagusNotReady` (Stage 6) so the pod's readiness, not the
HelmRelease's, is what pages.

**2. Readiness becoming real (Stage 5) turns a postgres blip into a total outage.**
Today `/readyz` returns `"ready"` unconditionally, so a CNPG hiccup degrades nagus to
500s on individual requests. After Stage 5 the readiness probe runs `pool.Ping`, and a
failure removes the *only* pod from the Service endpoints — nagus vanishes entirely,
including `/mcp` for every agent. This is the correct behaviour, but it converts a
partial degradation into a hard one at exactly the moment the shared database is
unhealthy, and there is currently no alert that would tell you (`NagusDown` fires on
`up == 0`, and a not-ready pod is still scraped, so `up` stays 1).
*Mitigations:* `failureThreshold: 3` + `periodSeconds: 10` (30s of tolerance);
`WithCheckTimeout(3s)`; keep liveness on `/healthz`; ship the `NagusNotReady` alert in
the SAME release as the readiness change, not in Stage 6 after it; and rehearse the
failure deliberately in a maintenance window before trusting it.

**3. Memory. The kit's dependency graph inside a 256Mi limit under `Recreate`.**
Stage 4 adds the OTel SDK, `contrib/instrumentation/runtime`, the Prometheus client
and (Stage 7) huma. RSS grows, and the current `resources.limits.memory: 256Mi` was
sized for a service with fifteen modules. An OOMKill under `Recreate` is a
CrashLoopBackOff with no surviving old pod — an outage, and one that looks like a code
bug rather than a resource-limit bug. It would also be invisible today: nagus has no
memory alert because it has no ServiceMonitor.
*Mitigations, in this order:* Stage 2 raises the limit to 384Mi and the request to
128Mi **before** the code that needs it; Stage 3 ships `NagusHighMemory` at 0.85 of
the limit **before** the code that needs it; Stage 4's verification includes an
explicit one-hour `container_memory_working_set_bytes` reading against the recorded
baseline. The ordering of stages 2 and 3 ahead of 4 exists entirely for this.

---

## 7. Beads to file before starting

**nagus:**
- Stage 1..8, one bead each, with this document as the reference.
- `helm.sh/resource-policy: keep` on the sqlite PVC (one line, independent).
- Decide the fate of the legacy sqlite PVC now that `backend: postgres`.

**go-service-kit:** one bead per numbered item in §5. Items 2, 4, 6 and 7 are the ones
that block or seriously tax a real service; 1, 3, 8, 9, 10 and 12 are documentation or
small API additions; 11 and 13 are design conversations.

**gitops:**
- No egress rule permits `openclaw -> nagus:8080` in
  `networkpolicies-openclaw.yaml`. Inert under Flannel; breaks both nagus consumers
  at the Cilium cutover, for a reason unrelated to this migration.
- `configmap-openclaw-nagus-delivery-script.yaml:19` hardcodes the nagus URL as a
  `:-` default, duplicating the CronJob's env at line 46. Two sources of truth for one
  URL. Collapse to one.
- The delivery CronJob's failure is silent (`|| exit 0`, no alert). Add a
  `kube_job_status_failed` / last-success alert so a nagus breakage is not discovered
  by noticing missing Telegram pings.
