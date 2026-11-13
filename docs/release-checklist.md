# Release checklist

Work through this in order for every release. No release has been cut yet; the first one is v0.1.0, never "v1". TierPlan follows Semantic Versioning, and breaking changes are allowed between 0.x minors.

## Before the first public image (once)

- [ ] **The repository is public.** The running application links to its source at `https://github.com/ianglennon/tierplan/tree/<commit>` (Settings > About and `/api/version`), and AGPL section 13 requires that offer to work for everyone who uses a modified or unmodified network service. While the repository is private that link answers 404 to outsiders. Make the repository public **before** the first public image is pushed, and check the link from a signed-out browser. (RESEARCH Pitfall 25.)
- [ ] The planning files (`.planning/`, `.claude/`, `CLAUDE.md`) are not in the repository history. They are git-ignored and must never be committed or pushed. Check with `git log --all --stat -- .planning .claude CLAUDE.md`.
- [x] **The pre-v0.1.0 migrations were squashed into one baseline** (D-85), `internal/store/migrations/00036_baseline.sql`, recorded in the release notes of v0.1.0. It keeps the number of the last development migration, so the next one is 00037. From this release on, a migration that has shipped is never edited again.
- [ ] GitHub Security Advisories are enabled on the repository, so SECURITY.md's reporting route works.

## Every release

- [ ] The branch to release from is `main`, and CI is **green**, including `make lint`, `make test`, `make gen-check`, `make licences`, `make spdx`, `make dco`, the Docker engine matrix (`make test-int`), `make release-check` and the end-to-end job (`make e2e`) and the proxy job (`make proxy-smoke`).
- [ ] `docs/` and the README still match the code: every `TIERPLAN_*` setting in `internal/config/config.go` is in the README table, and the README's `docker run` command matches `web/src/features/environments/docker-run-snippet.ts`.
- [ ] Dependencies changed? Run `make gen` and commit `web/public/third-party-notices.json`, and look at `make licences` output for new licences.
- [ ] The distroless base image digest in `Dockerfile` was bumped deliberately if you want newer base-image fixes (bump the digest, never the tag), and `make image image-smoke` passes with it.
- [ ] **Both images are built and released together.** The release workflow publishes `ghcr.io/ianglennon/tierplan` and `ghcr.io/ianglennon/tierplan-agent` from the same tag, each as `X.Y.Z` and `X.Y` for `linux/amd64` and `linux/arm64`, with no `:latest`. The agent image is the second signed image: verify it the way the server image is verified, with the same identity and issuer, and check its SBOM too:

  ```sh
  cosign verify ghcr.io/ianglennon/tierplan-agent:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-agent:X.Y.Z --format '{{ json .SBOM }}'
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-agent:latest   # must fail
  ```

  Install commands and automatic updates name the agent image by the server's own version, `ghcr.io/ianglennon/tierplan-agent:X.Y.Z`, so an unpublished or unsigned agent image breaks every new install and every update at once.
- [ ] **Agent and server versions match.** `ghcr.io/ianglennon/tierplan-agent:X.Y.Z` reports `vX.Y.Z` (start it once and read `starting tierplan-agent version=vX.Y.Z` in its log), the server reports the same in `/api/version`, and the compatibility window in `internal/tunnel/versionwindow.go` still accepts the previous minor: an agent of the previous release connects to this server and is offered the update.
- [ ] **The agent protocol is unchanged, or the change is deliberate.** The update and uninstall messages are frozen for all of 0.x so that any older agent that can connect can be updated out of its version. The golden files under `proto/wire/testdata` (`update`, `update_digest`, `update_result`, `update_result_error`, `uninstall` and `uninstall_service`) must be **unchanged** by the release (`git diff vPREVIOUS -- proto/wire/testdata` prints nothing); a diff there needs a decision recorded in the release notes, not a regenerated file.
- [ ] **The end-to-end run through a proxy passed on the release commit**, including the agent project: `make e2e` brings up TierPlan behind Caddy at `/tierplan`, runs the install command on a second Docker host, switches to it and revokes it. Look at the agent spec's trace in `.devdata/playwright-report` once: the waiting panel moves to "is online" without a reload, and the revoked page says "This environment was revoked". `make proxy-smoke` passed too.
- [ ] **Stacks (Phase 7) are proved on both engines and through the agent.** Each of these is in addition to the green CI run above:
  - **The Docker-backed jobs ran five times in a row on the release commit, all green, with no code change between them** (`scripts/ci-gate.sh` judges the series, see [testing.md](testing.md)). The stack tests deploy to real engines (the integration matrix, the swarm legs, the standalone deploys through a real agent and the end-to-end run), so a test that fails one run in five is a defect, not weather.
  - **`make test-swarm` passed on Engine 24 and on Engine 29** (`TIERPLAN_TEST_ENGINE=24` and `TIERPLAN_TEST_ENGINE=29`). The stack deploy is compared with `docker stack deploy` on a real swarm, and one case (a local-only image pinned to its image ID) skips on Engine 29 on purpose: read the skip reason, it is the open decision in [stacks.md](stacks.md#behaviour-that-is-still-open).
  - **The `stacks` project of `make e2e` ran** (look for `stacks.spec.ts` in `.devdata/test/e2e.json`): a stack is created, checked, reviewed, deployed, edited and removed on the managed host, and a stack with a secret file is deployed through a real agent and the secret is read inside the container.
  - **The agent image was rebuilt and published with this release, and the agents on your own hosts are updated.** A standalone stack needs the agent's container create, archive, rename and image inspect routes, which an agent from before Phase 7 does not have: a deploy to such a host is refused by the agent until it is updated (Settings > Environments, or [agent-updates.md](agent-updates.md)).
  - **`stacks.md` still says what the code does.** Its "Behaviour that is still open" and "What Phase 7 does not do" lists are true as written: an item that was decided or built since is removed there, and nothing deferred is described as shipped.
- [ ] **A real Let's Encrypt certificate replaces the self-signed one with an agent connected (DNS-01 on the dev swarm).** Pebble proves the code; only a real order proves the account, the zone and the chain a browser and an agent trust. The setup is the one in [https.md](https.md#lets-encrypt-with-dns-01-through-cloudflare).
  - **Preconditions.**
    - You are on the commit being released, the tree is clean and you have noted `git rev-parse HEAD`.
    - `env | grep TIERPLAN_ACME_` prints neither `TIERPLAN_ACME_DIRECTORY_URL` nor `TIERPLAN_ACME_CA_FILE`, so nothing points at a test CA.
    - A Cloudflare API token with Zone:DNS:Edit and Zone:Zone:Read for the zone is in a file only you can read.
    - The data directory is fresh: move the instance files (`tierplan.db` and its `-wal` and `-shm` files, `tierplan.key`, `tls/`, `backups/`, `git/`, `compose-config/`) aside, never delete them, and re-enrol the swarm's agents afterwards. A stored certificate that is not yet due makes Issue a no-op, and an account from an earlier run means no account row.
    - One order only. If it fails, report it and stop: never retry against production, because the duplicate-certificate limit is 5 per exact name set per 7 days.
  - **Steps**, in this order:
    1. Run `make dev` with `DEV_LISTEN=0.0.0.0:7480`, `DEV_SWARM_GATEWAY`, `DEV_PUBLIC_URL` and `DEV_WEB_HOST=0.0.0.0` set for this host. Do the setup, add the swarm environment and install its agent with the command the UI shows. `openssl s_client -connect dev.homelab.org.uk:7480 -servername dev.homelab.org.uk` shows the self-signed issuer, and the environment is online.
    2. In **Settings > HTTPS**: register the account, pick DNS-01, enter the token, press **Test token**, save the validation settings, then press **Issue certificate** once.
    3. The run panel's steps all end Done, except Account, which ends Skipped ("Account already registered.") because step 2 registered it. `openssl s_client` now shows a Let's Encrypt issuer and `Verify return code: 0`, and the page was not reloaded.
    4. The environment never left online while the certificate was swapped. After you restart the server it is online again and the Let's Encrypt certificate is still served.
    5. The audit page filtered to `https.` shows `https.account_registered`, `https.settings_updated`, `https.dns_tested`, `https.issue_requested` and `https.issued`, and an export of those rows holds no token.
    6. A read-only Cloudflare listing of the TXT records at `_acme-challenge.dev.homelab.org.uk` shows none, and none with a comment that starts with `tierplan:`.
    7. The bell has no HTTPS item.
    8. Optional: with HSTS off, **Stop using Let's Encrypt**. The agent stays connected over its pin.

  Last run: 2026-11-13 | build b6a3bd5d005e063472e63978a0434bfba446ee2d | pass: Let's Encrypt certificate issued and served, agents stayed connected through the swap and reconnected after a server restart
- [ ] Tag `vX.Y.Z` (for example `v0.1.0`) on the release commit and push the tag. Only a `v*` tag runs the release workflow.
- [ ] The release workflow finished: archives for `linux/amd64` and `linux/arm64`, `checksums.txt`, the image, and the signatures and SBOMs below.
- [ ] **Verify the signatures.** The workflow signs with keyless cosign, so the identity is the release workflow of this repository. Look at what `cosign verify` prints and confirm the identity and issuer are what you expect:

  ```sh
  cosign verify ghcr.io/ianglennon/tierplan:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

  cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

  These commands have not been run against a real release yet. If they fail, fix the command from the identity the certificate actually carries; do not loosen the pattern to match anything.
- [ ] **Check the SBOMs.** Each archive has a `.spdx.json` file next to it in the release. The image carries its own SPDX attestation:

  ```sh
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan:X.Y.Z --format '{{ json .SBOM }}'
  ```

  It must be present and list the Go and npm components.
- [ ] **Confirm the image tags.** `X.Y.Z` and `X.Y` exist (a pre-release gets only `X.Y.Z`), both for `linux/amd64` and `linux/arm64`, and **there is no `:latest` or other floating catch-all tag** while TierPlan is 0.x. `docker buildx imagetools inspect ghcr.io/ianglennon/tierplan:latest` must fail.
- [ ] Pull the released image on a clean machine and run the README's install command. It must be healthy, show the setup wizard, and report the right version in Settings > About.
- [ ] **Update the README's image tag from `:edge` to `:X.Y`** (and the upgrade example), in the same release, so the documented command pins the line it was written for.
- [ ] The GitHub release notes group changes by Conventional Commit type; read them and add anything a user must do when upgrading (a breaking change, a new setting, a migration).
