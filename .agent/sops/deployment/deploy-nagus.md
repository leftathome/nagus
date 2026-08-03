# SOP: deploy a nagus change

Verified end-to-end many times on 2026-07-31/08-01/08-02.

## 1. Land the code

```bash
go vet ./... && go test ./... -count=1 -race
go run honnef.co/go/tools/cmd/staticcheck@latest ./...
gofmt -l .            # must be empty
helm lint charts/nagus
git push origin main && git push github main   # BOTH: push-mirroring is not configured
```

## 2. Wait for CI, and expect the amd64 flake

GitLab builds on every push to main. The amd64 kaniko leg **OOMs
intermittently** (`exit code 137`) under node memory pressure; arm64 usually
passes. Retry the failed job — it has succeeded on retry every time so far.

Verify the artifacts exist before touching gitops:

```bash
# image tag is the 8-char SHA, NOT 7
git rev-parse --short=8 HEAD
# GitLab API: registry/repositories/84 = image, 86 = chart
```

## 3. Update gitops

`steve/gitops` → `clusters/orac/apps/nagus/helmrelease-nagus.yaml`:
set `image.tag` (8-char SHA) and, if templates changed, `chart.spec.version`.

**Other people work in this repo.** Use `git -c rebase.autostash=true pull
--rebase` so their uncommitted work survives; do not blanket-stash.

## 4. Reconcile and VERIFY IT ACTUALLY APPLIED

```bash
flux reconcile source git flux-system
flux reconcile kustomization apps
flux reconcile helmrelease nagus -n nagus
kubectl rollout status deployment/nagus -n nagus
```

`rollout status` reporting success is **not** proof your change applied — it
describes whatever rollout is current. Confirm the pod is newer than the config:

```bash
kubectl get pods -n nagus -o jsonpath='{.items[0].metadata.creationTimestamp}'
kubectl get cm nagus-config -n nagus -o jsonpath='{.metadata.managedFields[0].time}'
```

Since chart 0.5.1 a config-only change rolls the pod automatically (checksum
annotations). If the pod is OLDER than the ConfigMap, the change did not take —
`kubectl rollout restart deployment/nagus -n nagus`.

## 5. Confirm behaviour, not just health

```bash
kubectl logs -n nagus deploy/nagus | grep -e 'serve:' -e 'TRUNCATED'
```

Each source should report `fetched=N stored=N ... offers=N`. An offer-only source
shows `stored=0` by design. A `TRUNCATED` line means a page cap is cutting a
catalogue short.

## Querying prod

The service is ClusterIP on **port 8080** (not 80). The PVC is RWO and already
mounted, so a second pod cannot mount it — query over HTTP instead:

```bash
kubectl run q -n nagus --rm -i --restart=Never --image=docker.io/curlimages/curl:latest \
  -- curl -s 'http://nagus.nagus.svc.cluster.local:8080/search?category=hdd&limit=20'
```

For SQL, sync the Vault password into a throwaway ExternalSecret
(`eso/nagus/infrastructure` → `postgres_password`), run a `postgres:17-alpine`
pod, and delete both afterwards. The local `vault` CLI token is expired; ESO's
own auth works.
