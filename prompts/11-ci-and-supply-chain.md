# 11 · CI and supply-chain hygiene

## Context
`.github/workflows/ci.yml` runs `go test -race ./...`, `go vet ./...`, builds the image, and smoke-tests `/healthz` against a read-only container. `publish.yml` publishes a multi-arch image to GHCR on `v*` tags and already sets `provenance: true` and `sbom: true`. The image is built in `golang:1.27-alpine` with `CGO_ENABLED=0` and runs from `scratch` as UID 10001. Dependencies are in `go.mod`/`go.sum`. Dependabot covers `gomod`, `docker`, and `github-actions`.

## Findings (verified at `4bafb11`)
1. Actions are pinned by mutable tag (`actions/checkout@v4`, `actions/setup-go@v5`, `docker/*@v3/v5/v6`), not by commit SHA. `publish.yml` has `packages: write`.
2. No static analysis beyond `go vet`; no `gofmt` check; no `staticcheck`/`golangci-lint`; no `govulncheck`; no coverage report.
3. `FROM golang:1.27-alpine` is a floating tag, and the README tells operators to prefer image digests in production, but the repo's own Dockerfile does not pin by digest. (The final stage is `scratch`, so there is no runtime base image to pin, but the build stage and the CA bundle source are.)
4. `publish.yml` re-runs tests but does **not** build the image and smoke-test it before pushing, so a broken image can be published.
5. No image vulnerability scan. A Trivy or Grype scan of a `scratch` image still reads the Go build info embedded in the binary, so it can report module vulnerabilities; `govulncheck` is more precise because it checks reachability.
6. The release version is not injected into the binary (`main.go` prints a hardcoded "Next Chapter 0.2.0 (Go)"), and nothing checks that the git tag matches. See prompt 08 for the `Version` variable and `-ldflags`.
7. `deploy/kubernetes/app.yaml` pins `ghcr.io/lafronzt/next-chapter:0.2.0` (good), but nothing validates the manifest. There is no manifest linting.
8. CI has no job that runs `go mod tidy` / `go mod verify` and checks for drift.
9. `go.mod` says `go 1.27.0`; CI uses `go-version-file: go.mod`, so there is no separate "supported version floor" to test. Confirm that and do not add a matrix unless it is useful.

## Task
1. Pin every third-party action by full commit SHA with a trailing `# vX.Y.Z` comment, in both workflows. Keep Dependabot's `github-actions` ecosystem enabled (and consider grouping updates).
2. Add a `lint` job: `gofmt -l .` must print nothing, `go vet ./...`, `staticcheck ./...` (install a pinned version through `go run honnef.co/go/tools/cmd/staticcheck@<version>` or use the official action pinned by SHA), `go mod tidy` followed by `git diff --exit-code go.mod go.sum`, and `go mod verify`. Fix real findings only; do not reformat unrelated code.
3. Add `govulncheck ./...` as a job (pinned version). Make it fail on reachable vulnerabilities. Add a coverage step (`go test -race -coverprofile=… ./...` and `go tool cover -func`) that prints a summary and uploads the profile as an artifact without failing the build.
4. Dockerfile: pin `golang:1.27-alpine` by digest with a comment of the tag, keep `CGO_ENABLED=0`, `-trimpath`, and `-ldflags`, and add `-X` for the version (coordinate with prompt 08), driven by a build arg that CI sets from the tag or `git describe`. Let Dependabot's `docker` ecosystem bump the digest.
5. `publish.yml`: build the image first (load it locally), run the same container smoke test as CI against it, verify the tag matches the embedded version (`next-chapter version`), and only then push. Keep `provenance` and `sbom`. Add a Trivy (or Grype) scan of the built image that fails on fixable HIGH/CRITICAL findings. Keep `permissions` least-privilege per job (`contents: read` for build/test, `packages: write` only on the push job); consider also `id-token: write` + `attestations: write` only if you add build attestations.
6. Add `kubeconform` (pinned) validation of `deploy/kubernetes/app.yaml` to CI, and a check that the manifest image tag is not `latest`.
7. De-duplicate the smoke test: put it in a small script (`scripts/smoke.sh`) used by both workflows so they cannot drift.
8. Document in CONTRIBUTING: the required local checks (`gofmt -l .`, `go vet ./...`, `staticcheck ./...`, `go test -race ./...`) and how to bump an action SHA and a base-image digest.

## Acceptance criteria
- You can't run GitHub Actions locally. Validate YAML syntax (`yq`/`actionlint` if available), run `gofmt`, `go vet`, `staticcheck`, `govulncheck`, `go mod tidy` locally, and `docker build` plus the smoke script locally if Docker is available. State exactly what was and was not verified.
- Workflows keep least-privilege `permissions`.
- Nothing requires a paid service, and no secrets beyond `GITHUB_TOKEN`.
