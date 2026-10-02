# Release checklist

Work through this in order for every release. No release has been cut yet; the first one is v0.1.0, never "v1". Bayplan follows Semantic Versioning, and breaking changes are allowed between 0.x minors.

## Before the first public image (once)

- [ ] **The repository is public.** The running application links to its source at `https://github.com/ianglennon/bayplan/tree/<commit>` (Settings > About and `/api/version`), and AGPL section 13 requires that offer to work for everyone who uses a modified or unmodified network service. While the repository is private that link answers 404 to outsiders. Make the repository public **before** the first public image is pushed, and check the link from a signed-out browser. (RESEARCH Pitfall 25.)
- [ ] The planning files (`.planning/`, `.claude/`, `CLAUDE.md`) are not in the repository history. They are git-ignored and must never be committed or pushed. Check with `git log --all --stat -- .planning .claude CLAUDE.md`.
- [ ] **Decide whether to squash the pre-v0.1.0 migrations into one baseline before the first tag** (D-85). Once a tagged release ships a migration, users' databases depend on it and it is never edited again, so this choice is one-way. The current migrations are `internal/store/migrations/00001` to `00006`; record the decision (squash, or keep as is) in the release notes of v0.1.0.
- [ ] GitHub Security Advisories are enabled on the repository, so SECURITY.md's reporting route works.

## Every release

- [ ] The branch to release from is `main`, and CI is **green**, including `make lint`, `make test`, `make gen-check`, `make licences`, `make spdx`, `make dco`, the Docker engine matrix (`make test-int`), `make release-check` and the end-to-end job (`make e2e`).
- [ ] `docs/` and the README still match the code: every `BAYPLAN_*` setting in `internal/config/config.go` is in the README table, and the README's `docker run` command matches `web/src/features/environments/docker-run-snippet.ts`.
- [ ] Dependencies changed? Run `make gen` and commit `web/public/third-party-notices.json`, and look at `make licences` output for new licences.
- [ ] The distroless base image digest in `Dockerfile` was bumped deliberately if you want newer base-image fixes (bump the digest, never the tag), and `make image image-smoke` passes with it.
- [ ] Tag `vX.Y.Z` (for example `v0.1.0`) on the release commit and push the tag. Only a `v*` tag runs the release workflow.
- [ ] The release workflow finished: archives for `linux/amd64` and `linux/arm64`, `checksums.txt`, the image, and the signatures and SBOMs below.
- [ ] **Verify the signatures.** The workflow signs with keyless cosign, so the identity is the release workflow of this repository. Look at what `cosign verify` prints and confirm the identity and issuer are what you expect:

  ```sh
  cosign verify ghcr.io/ianglennon/bayplan:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/bayplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

  cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
    --certificate-identity-regexp '^https://github.com/ianglennon/bayplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

  These commands have not been run against a real release yet. If they fail, fix the command from the identity the certificate actually carries; do not loosen the pattern to match anything.
- [ ] **Check the SBOMs.** Each archive has a `.spdx.json` file next to it in the release. The image carries its own SPDX attestation:

  ```sh
  docker buildx imagetools inspect ghcr.io/ianglennon/bayplan:X.Y.Z --format '{{ json .SBOM }}'
  ```

  It must be present and list the Go and npm components.
- [ ] **Confirm the image tags.** `X.Y.Z` and `X.Y` exist (a pre-release gets only `X.Y.Z`), both for `linux/amd64` and `linux/arm64`, and **there is no `:latest` or other floating catch-all tag** while Bayplan is 0.x. `docker buildx imagetools inspect ghcr.io/ianglennon/bayplan:latest` must fail.
- [ ] Pull the released image on a clean machine and run the README's install command. It must be healthy, show the setup wizard, and report the right version in Settings > About.
- [ ] **Update the README's image tag from `:edge` to `:X.Y`** (and the upgrade example), in the same release, so the documented command pins the line it was written for.
- [ ] The GitHub release notes group changes by Conventional Commit type; read them and add anything a user must do when upgrading (a breaking change, a new setting, a migration).
