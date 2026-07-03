# Releasing nagus

nagus follows [Semantic Versioning](https://semver.org). A release is a git tag
`vX.Y.Z`; pushing it triggers the build + publish pipelines. The chart's
`version` and `appVersion` track the release.

## Cutting a release

1. Ensure `main` is green: `go vet ./... && go test ./... -count=1 -race`.
2. Update `CHANGELOG.md` -- move the pending notes under a new `## [X.Y.Z]`
   heading with the date.
3. Bump `charts/nagus/Chart.yaml` `version` **and** `appVersion` to `X.Y.Z`
   (on a tag the pipelines stamp the chart from the tag anyway, but keep the
   committed values in sync so a plain `main` build publishes the right number).
4. Commit (`chore(release): vX.Y.Z`) and push `main`.
5. Tag and push:

   ```
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

   Use the `v` prefix -- the GitHub image/release workflows trigger on `v*`.

## What the tag builds

**GitLab (primary, `gitlab.orac.local`)** -- `.gitlab-ci.yml`, on `$CI_COMMIT_TAG`:

| stage | artifact |
|---|---|
| `test` | vet + `-race` gate |
| `build` | kaniko image -> the in-cluster GitLab Container Registry |
| `chart` | `charts/nagus` -> OCI chart, version stamped from the tag |
| `release` | a GitLab Release for the tag |

Homelab specifics (kaniko CA workaround, registry Service, pod sizing) live in
the private `homelab/ci-templates` include; this file carries none.

**GitHub (downstream public mirror)** -- publishes the deployable artifacts the
HelmRelease consumes:

| workflow | artifact |
|---|---|
| `docker.yml` | `ghcr.io/leftathome/nagus:X.Y.Z` + `:latest` (amd64/arm64) |
| `ci.yml` (helm job) | chart -> `oci://ghcr.io/leftathome/charts/nagus:X.Y.Z` |
| `release.yml` | cross-platform binaries + checksums on a GitHub Release |

## After tagging

- Watch the GitLab pipeline for the tag (build -> chart -> release).
- Confirm the image and chart exist:

  ```
  helm pull oci://ghcr.io/leftathome/charts/nagus --version X.Y.Z
  ```

- Roll the deployment by bumping the HelmRelease `chart.spec.version` to
  `X.Y.Z` in the gitops repo (see [DEPLOY.md](DEPLOY.md)).

## Version stamping

The binary reports its version via `nagus version`; it is injected at build time
(`-ldflags "-X main.version=..."`, from the `VERSION` build-arg / tag). An
untagged build reports `dev`.
