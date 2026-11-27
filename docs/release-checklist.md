# Release checklist

Work through this in order for every release. TierPlan follows Semantic Versioning, and breaking changes are allowed between 0.x minors. The first release was v0.1.0, never "v1"; v0.1.1 followed it, and every release since runs through this list.

## Before the first public image (once)

- [ ] **The repository is public.** The running application links to its source at `https://github.com/ianglennon/tierplan/tree/<commit>` (Settings > About and `/api/version`), and AGPL section 13 requires that offer to work for everyone who uses a modified or unmodified network service. While the repository is private that link answers 404 to outsiders. Make the repository public **before** the first public image is pushed, and check the link from a signed-out browser. (RESEARCH Pitfall 25.)
- [ ] The planning files (`.planning/`, `.claude/`, `CLAUDE.md`) are not in the repository history. They are git-ignored and must never be committed or pushed. Check with `git log --all --stat -- .planning .claude CLAUDE.md`.
- [x] **The pre-v0.1.0 migrations were squashed into one baseline** (D-85), `internal/store/migrations/00036_baseline.sql`, recorded in the release notes of v0.1.0. It keeps the number of the last development migration, so the next one is 00037. From this release on, a migration that has shipped is never edited again.
- [ ] GitHub Security Advisories are enabled on the repository, so SECURITY.md's reporting route works.

## Every release

- [ ] The branch to release from is `main`, and CI is **green**, including `make lint`, `make test`, `make gen-check`, `make licences`, `make spdx`, `make dco`, the Docker engine matrix (`make test-int`), `make release-check` and the end-to-end job (`make e2e`) and the proxy job (`make proxy-smoke`).
- [ ] `docs/` and the README still match the code: every `TIERPLAN_*` setting in `internal/config/config.go` is in the README table, and the README's `docker run` command matches `web/src/features/environments/docker-run-snippet.ts`.
- [ ] Dependencies changed? Run `make gen` and commit `web/public/third-party-notices.json`, and look at `make licences` output for new licences.
- [ ] The distroless base image digest was bumped deliberately if you want newer base-image fixes (bump the digest, never the tag). `Dockerfile`, `agent/Dockerfile`, `ingress/Dockerfile` and `keepalived/Dockerfile` share one digest and it is bumped in all four together (`keepalived/Dockerfile` reads its digest from its last `FROM` line, because its first stage is the Alpine build base); `scripts/ingress-pin-check.sh`, part of `make lint`, fails when they differ. Run `make image image-smoke ingress-image ingress-image-smoke keepalived-image keepalived-image-smoke` with it. The Alpine build base of `keepalived/Dockerfile` is pinned by its own digest, which is bumped deliberately and never by tag.
- [ ] **The Traefik pin was checked.** Look at the Traefik 3.7.x line (its releases and its security advisories) for a newer patch than the one in `ingress/Dockerfile`. A bump changes `ingress/Dockerfile` and `internal/ingressrender/traefiktest.TraefikImage` together, tag and index digest. It also updates the licence evidence the image ships: `ingress/image/doc/NOTICE.md` (the version, the source URL, the commit the tag points at, and the count and wording of the reviewed exceptions) and `ingress/image/doc/LICENSE.md` (copy `LICENSE.md` from the new tag), and re-reviews `licence-exceptions.txt`, whose Traefik entries are tied to the version they were reviewed against. Then `scripts/ingress-pin-check.sh` (it fails when the notice names another version), the Traefik test harness (`go test -tags=integration ./internal/ingressrender/...`) and `make licences` (it fails when the tag's commit differs from the notice or its `LICENSE.md` from the shipped one) must pass. There is no bot that does this for you. A Traefik security release means a TierPlan patch release, so cut one rather than waiting for the next minor.
- [ ] **All four images are built and released together.** The release workflow publishes `ghcr.io/ianglennon/tierplan`, `ghcr.io/ianglennon/tierplan-agent`, `ghcr.io/ianglennon/tierplan-ingress` and `ghcr.io/ianglennon/tierplan-keepalived` from the same tag, each as `X.Y.Z` and `X.Y` for `linux/amd64` and `linux/arm64`, with no `:latest`. The agent, ingress and keepalived images are the second, third and fourth signed images: verify each the way the server image is verified, with the same identity and issuer, and check its SBOM too. For the agent:

  ```sh
  cosign verify ghcr.io/ianglennon/tierplan-agent:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-agent:X.Y.Z --format '{{ json .SBOM }}'
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-agent:latest   # must fail
  ```

  And the same three lines for the ingress image:

  ```sh
  cosign verify ghcr.io/ianglennon/tierplan-ingress:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-ingress:X.Y.Z --format '{{ json .SBOM }}'
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-ingress:latest   # must fail
  ```

  And the same three lines for the keepalived image:

  ```sh
  cosign verify ghcr.io/ianglennon/tierplan-keepalived:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-keepalived:X.Y.Z --format '{{ json .SBOM }}'
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-keepalived:latest   # must fail
  ```

  Install commands and automatic updates name the agent image by the server's own version, `ghcr.io/ianglennon/tierplan-agent:X.Y.Z`, so an unpublished or unsigned agent image breaks every new install and every update at once. The ingress deployer of Phase 24 names the ingress image by the server's version in the same way, so an unpublished or unsigned ingress image breaks every ingress switch-on. The virtual IP of Phase 26 names the keepalived image by the server's version in the same way, so an unpublished or unsigned keepalived image breaks every virtual IP switch-on.
- [ ] **The keepalived source tarball is attached to the release, and it is the one the image was built from.** keepalived is GPL-2.0-or-later and the tarball is the source offer for the binary in `tierplan-keepalived`, so a release without it is not releasable. Check, on the release page, that `keepalived-2.4.3.tar.gz` is attached and that it is listed in `checksums.txt` (whose signature is verified below). Then:

  ```sh
  sha256sum keepalived-2.4.3.tar.gz   # must equal the hash in keepalived/Dockerfile and scripts/keepalived-source.sh
  ```

  and read the same hash in the image's label: pull `ghcr.io/ianglennon/tierplan-keepalived:X.Y.Z` and run `docker inspect --format '{{ index .Config.Labels "org.tierplan.keepalived.sha256" }}'` on it. The image also holds the licence texts at `/licenses/keepalived/COPYING` and `/licenses/musl/COPYRIGHT` and a notice at `/usr/share/doc/tierplan-keepalived/NOTICE.md`; `make keepalived-image-smoke` checks all three before the release.
- [ ] **A keepalived version bump is deliberate and changes every file that names the version together.** `keepalived/Dockerfile` (the version in the path, the hash and the labels), `scripts/keepalived-source.sh` (version, URL and hash), the SBOM template `keepalived/image/sbom/keepalived.spdx.json.in` (the version, the download location, the hash and the document namespace), the notice `keepalived/image/doc/NOTICE.md`, the shape test `keepalived/dockerfile_test.go`, `scripts/keepalived-image-smoke.sh`, the tarball paths in `.goreleaser.yaml` (three) and the two `cp` lines of the `Makefile`. Find them all with `git grep -n '2\.4\.3'` before you start, and again after. Then `go test ./keepalived/...`, `make keepalived-image keepalived-image-smoke` and `make release-check` must pass. The CI cache of the tarball is keyed by `scripts/keepalived-source.sh`, so the bump fetches the new one. When the build says the linked musl is another version, replace `keepalived/image/licenses/musl/COPYRIGHT` with that version's file and change the version in `keepalived/Dockerfile` and the version and the hash in the shape test. A keepalived security release means a TierPlan patch release, so cut one rather than waiting for the next minor.
- [ ] **Agent and server versions match.** `ghcr.io/ianglennon/tierplan-agent:X.Y.Z` reports `vX.Y.Z` (start it once and read `starting tierplan-agent version=vX.Y.Z` in its log), the server reports the same in `/api/version`, and the compatibility window in `internal/tunnel/versionwindow.go` still accepts the previous minor: an agent of the previous release connects to this server and is offered the update.
- [ ] **The agent protocol is unchanged, or the change is deliberate.** The update and uninstall messages are frozen for all of 0.x so that any older agent that can connect can be updated out of its version. The golden files under `proto/wire/testdata` (`update`, `update_digest`, `update_result`, `update_result_error`, `uninstall` and `uninstall_service`) must be **unchanged** by the release (`git diff vPREVIOUS -- proto/wire/testdata` prints nothing); a diff there needs a decision recorded in the release notes, not a regenerated file.
- [ ] **The end-to-end run through a proxy passed on the release commit**, including the agent project: `make e2e` brings up TierPlan behind Caddy at `/tierplan`, runs the install command on a second Docker host, switches to it and revokes it. Look at the agent spec's trace in `.devdata/playwright-report` once: the waiting panel moves to "is online" without a reload, and the revoked page says "This environment was revoked". `make proxy-smoke` passed too.
- [ ] **Stacks (Phase 7) are proved on both engines and through the agent.** Each of these is in addition to the green CI run above:
  - **`make test-swarm` passed on Engine 24 and on Engine 29** (`TIERPLAN_TEST_ENGINE=24` and `TIERPLAN_TEST_ENGINE=29`). The stack deploy is compared with `docker stack deploy` on a real swarm, and one case (a local-only image pinned to its image ID) skips on Engine 29 on purpose: read the skip reason, it is the open decision in [stacks.md](stacks.md#behaviour-that-is-still-open).
  - **The `stacks` project of `make e2e` ran** (look for `stacks.spec.ts` in `.devdata/test/e2e.json`): a stack is created, checked, reviewed, deployed, edited and removed on the managed host, and a stack with a secret file is deployed through a real agent and the secret is read inside the container.
  - **The agent image was rebuilt and published with this release, and the agents on your own hosts are updated.** A standalone stack needs the agent's container create, archive, rename and image inspect routes, which an agent from before Phase 7 does not have: a deploy to such a host is refused by the agent until it is updated (Settings > Environments, or [agent-updates.md](agent-updates.md)).
  - **`stacks.md` still says what the code does.** Its "Behaviour that is still open" and "What Phase 7 does not do" lists are true as written: an item that was decided or built since is removed there, and nothing deferred is described as shipped.
- [ ] **A real Let's Encrypt certificate replaces the self-signed one with an agent connected (DNS-01 on the dev swarm).** Pebble proves the code; only a real order proves the account, the zone and the chain a browser and an agent trust. The setup is the one in [https.md](https.md#lets-encrypt-with-dns-01-through-cloudflare).
  - **Preconditions.**
    - You are on the commit being released, the tree is clean and you have noted `git rev-parse HEAD`.
    - `env | grep TIERPLAN_ACME_` prints neither `TIERPLAN_ACME_DIRECTORY_URL` nor `TIERPLAN_ACME_CA_FILE`, so nothing points at a test CA.
    - A Cloudflare API token with Zone:DNS:Edit and Zone:Zone:Read for the zone is in a file only you can read.
    - The data directory is fresh: move the instance files (`tierplan.db` and its `-wal` and `-shm` files, `tierplan.key`, `tls/`, `backups/`, `git/`, `compose-config/`) aside, never delete them, and re-enrol the swarm's agents afterwards: before step 1's install command, remove the old agent on a manager with the swarm command in [agent.md](agent.md#reinstall-an-agent-on-a-host-that-ran-one) (it finds the agent service by its `tierplan.agent=true` label) and keep the `tierplan-agent` volume, or the install command refuses because an agent service exists. A stored certificate that is not yet due makes Issue a no-op, and an account from an earlier run means no account row.
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

  Last run: 2026-11-13 | build c9dde2b70f0f254a512d959ab394f67f090088e2 | pass: Let's Encrypt certificate issued and served, agents re-enrolled by themselves after a guarded reinstall, stayed connected through the swap and reconnected after a server restart
- [ ] **What Phases 18 to 28 add: ingress. A labelled stack and TierPlan itself are served through the ingress with real certificates, and the HLD's failure table holds.** Pebble and the Traefik test harness prove the code; only a run on a real swarm with a real zone proves that the virtual IP moves across a real switch, that a production certificate chains for a client that verifies it, and that TierPlan stays reachable when its own route is off or broken. The setup is the one in [ingress.md](ingress.md) and [install-in-swarm.md](install-in-swarm.md).
  - **Preconditions.**
    - You are on the commit being released, the tree is clean and you have noted `git rev-parse HEAD`.
    - The images are built from that commit (`make image`, `make ingress-image`, `make keepalived-image`, `make agent-image`) and loaded into the nodes of the rig. The rig is the throwaway three-node swarm of `scripts/vip-l2-rig.sh` on a macvlan network, so the address is a real one on a real switch. It never touches the persistent dev swarm.
    - The rig's addresses were approved by the maintainer, and `scripts/vip-l2-rig.sh check-free` ran first and said they are unused (`VIP_L2_ADDRESS`, `VIP_L2_RANGE`, `VIP_L2_PARENT` and `VIP_L2_OBSERVER` are set; the observer is another machine on the same LAN). Then `scripts/vip-l2-rig.sh up`.
    - TierPlan is installed inside the rig's swarm as a service, from the stack file in [install-in-swarm.md](install-in-swarm.md). The `tierplan-self` lines are **not** in the file yet: they are added only after the self route is switched on (step 4).
    - The Cloudflare token was checked first with the verify call (`.devdata/uat-17/cfget.sh tokens/cf /user/tokens/verify`), reading only its status and expiry. If it expires before the run can finish, stop and ask for a renewed token.
    - One single-attempt production Let's Encrypt order **per hostname**, for two hostnames: the labelled stack's, and TierPlan's own route's. Each order is made once and **never retried**. A hostname whose order fails is withdrawn at once (remove its route labels, or press **Turn off own route**), so that the issuance governor cannot retry it, and the failure is reported and the run stops there: the duplicate-certificate limit is 5 per exact name set per 7 days, and 50 per registered domain per 7 days.
    - Evidence goes under `.devdata/`. A token is **never printed** in it: not in a command, a log excerpt or a screenshot, and Playwright failure artifacts are not opened when a step typed a token (list their file names only).
  - **Steps**, in this order, each with the result to expect:
    1. In the Ingress page, switch ingress on for the swarm, register the apps domain with the token, and switch the virtual IP on with the rig's address. Expect the job to finish, the proxy to be enrolled on all three nodes, and the virtual IP to be held by exactly one node (`scripts/vip-l2-rig.sh holder`).
    2. Deploy a stack with one labelled hostname under the apps domain (`tierplan.ingress.host: <host>`, as in [ingress.md](ingress.md)). Expect the route to reach Serving once its one production order is done, with no retry.
    3. From the observer machine, `curl --resolve <host>:443:<VIP> https://<host>/`, without `-k`, answers from the stack, and `openssl s_client -connect <VIP>:443 -servername <host> </dev/null 2>/dev/null | openssl x509 -noout -issuer` prints a Let's Encrypt production issuer. A client that verifies the chain is the point: a self-signed or staging issuer is a failure.
    4. **The self route.** On the Ingress page, Settings tab, press **Turn on own route** on the card "TierPlan's own route", and give a hostname under the apps domain. Expect the review to list its five items in order (hostname check, domain and certificate state, self-network attachment, the origin that will be added, and the HTTP-01 carve-out when it applies), and, once confirmed, the card to say **Not attached**. Add the `tierplan-self` lines the card shows to the stack file and deploy it again (this restarts TierPlan's task, and the routes of step 2 keep serving meanwhile). Expect the card to reach **Serving**, and Settings > HTTPS to show the line "TierPlan is also served at ... through ingress." From the observer, `curl --resolve <route host>:443:<VIP> https://<route host>/` without `-k` answers with TierPlan's sign-in page and a Let's Encrypt production issuer, from the route's own single production order (the second of the two).
    5. **The real client address.** Sign in through the route from another machine. Expect the audit page to show that machine's address on the sign-in row, not the proxy's and not a `tierplan-self` address.
    6. **A state change on 7480.** On the direct address (`https://<node>:7480`), change a setting, which is a state-changing request. Expect it to save, with no origin or CSRF refusal.
    7. **The agent and the proxy keep dialling directly.** The agent's log shows its connection to the direct URL, and the node list shows the direct address for each node. `curl https://<route host>/agent/connect` through the route answers 404.
    8. **TierPlan's own HTTP-01.** With port 80 reachable from the internet, a renewal of TierPlan's own certificate goes through the port-80 carve-out. Where port 80 is not reachable from the internet (the LAN rig is not), run the S12 integration test (`go test -tags=integration ./internal/ingressrender/...`) or a Pebble order instead, and say in the run record which one was used and why.
    9. **The failure table of the ingress design (HLD section 6)**, one sub-step each, with the expected result:
       1. TierPlan's service scaled to 0 (`docker service scale <stack>_tierplan=0`): the routes of steps 2 and 4 keep serving. They also keep serving across a proxy restart (`docker service update --force tierplan-ingress`) and across a node reboot (restart one node of the rig and wait for it to return). Then scale TierPlan back to 1 and expect it to come up and the status to recover.
       2. Kill the holder of the virtual IP (`scripts/vip-l2-rig.sh kill-holder`): the address moves to another node within seconds (`scripts/vip-l2-rig.sh wait-move <node>` prints the seconds, and `observe` names the new holder), and the routes still serve from the observer.
       3. Stop Traefik in one proxy task (kill its process in that task): the supervisor restarts it from the cache and that node's routes come back without TierPlan's help.
       4. Push a malformed bundle (the supervisor's existing test path, or a hand-made one): it is rejected, and the cache keeps serving the last good one.
       5. A failed issuance holds the route in **Waiting for certificate**, with a reason, while the deploy stays successful. Cause it without spending a production order: remove the token's write permission or point the route at a hostname whose challenge record cannot be written, so the order fails before it reaches the CA, or use the Pebble test CA. Never retry a failed production order; withdraw the hostname.
       6. Misconfigure ingress (for example remove the domain's token): TierPlan still answers on 7480, and the sign-in and the pages work there. Put the token back afterwards.
    10. Press **Turn off own route** on 7480. Expect the route to stop answering on `<route host>`, and 7480 to keep working, with the Settings > HTTPS line gone and the `tierplan-self` lines now safe to remove.
    11. **Tear down.** Switch the virtual IP and ingress off, remove the stack and the `tierplan-self` lines, then `scripts/vip-l2-rig.sh down`. Expect nothing left on the dev swarm and no `tp-l2-*` container or network on this machine (`docker ps -a --filter label=tierplan.l2-rig=true` prints nothing). No token is left in the evidence, and `go clean -cache` has run on this host and on the CI runners.

  Last run: 2026-11-27 | build e3057d2db33e31a11f7ea405cf8a199c2f1da16a | issues, all minor and none blocked a step: both routes served on their own production certificates and verified from another machine, routes kept serving with TierPlan stopped across a proxy restart and a node reboot, the virtual IP moved in 3.4 s, and 7480 kept working with ingress broken. Open: TierPlan's own route drops for about 45 s after TierPlan restarts, a false "Requests don't match the public URL" warning names the self route's host, the node status stays ready while the proxy service is gone, and switching the self route on or off moves the virtual IP (1 to 2 s of misses on every route)
- [ ] Tag `vX.Y.Z` (for example `v0.1.0`) on the release commit and push the tag. Only a `v*` tag runs the release workflow.
- [ ] The release workflow finished: archives for `linux/amd64` and `linux/arm64`, `checksums.txt`, the four images (server, agent, ingress and keepalived), the keepalived source tarball, and the signatures and SBOMs below.
- [ ] **Verify the signatures.** The workflow signs with keyless cosign, so the identity is the release workflow of this repository. Verifying needs **cosign v3** (`cosign version` must report v3). The release workflow installs cosign with `sigstore/cosign-installer` v4.1.2, whose default cosign is v3, and `sign-blob --bundle` writes the `.sigstore.json` bundle format that v3 reads: signed with v3, verify with v3. An older cosign can fail on the bundle or on the image signature for that reason alone. Look at what `cosign verify` prints and confirm the identity and issuer are what you expect. The commands below are for the server image and the archive checksums; the agent, ingress and keepalived images are verified above:

  ```sh
  cosign verify ghcr.io/ianglennon/tierplan:X.Y.Z \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com

  cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
    --certificate-identity-regexp '^https://github.com/ianglennon/tierplan/\.github/workflows/release\.yml@refs/tags/v' \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

  If a command fails, fix the command from the identity the certificate actually carries; do not loosen the pattern to match anything.
- [ ] **Check the SBOMs.** Each archive has a `.spdx.json` file next to it in the release. The image carries its own SPDX attestation:

  ```sh
  docker buildx imagetools inspect ghcr.io/ianglennon/tierplan:X.Y.Z --format '{{ json .SBOM }}'
  ```

  It must be present and list the Go and npm components, and the npm count is checked, not assumed:
  - Run `make web-sbom`. It prints how many npm packages the web interface has and fails when that list and `pnpm licenses` differ.
  - The server archive's `.spdx.json` (each archive also holds the web interface's own `sbom/web.spdx.json`) must list at least that many packages with a `pkg:npm/` locator. The release already stops before anything is signed or published when any one npm package of the web SBOM is absent from it (`scripts/sbom-archive.sh`, a set comparison, not a count), so a short list here means the gate was bypassed.
  - The server image attestation must list at least as many. Count the distinct `pkg:npm/` locators in its packages' `externalRefs` (the output is keyed by platform on a multi-platform image, so the filter walks the whole document) and compare the number with `make web-sbom`:

    ```sh
    docker buildx imagetools inspect ghcr.io/ianglennon/tierplan:X.Y.Z --format '{{ json .SBOM }}' \
      | jq '[.. | .packages? // empty | .[] | .externalRefs[]? | .referenceLocator | select(startswith("pkg:npm/"))] | unique | length'
    ```

  - The agent, ingress and keepalived images contain no web interface, so their SBOMs list Go modules (and, for keepalived, the two entries below) only. That is correct: npm packages listed there would make them false.
  - The ingress image SBOM must list `github.com/traefik/traefik/v3`, taken from the Traefik binary's build information. Its licences show NOASSERTION because Go build information carries none; the licence evidence for Traefik is the Traefik leg of `make licences`. Run the filter below. It must print one line, `pkg:golang/github.com/traefik/traefik/v3@` followed by the tag that `scripts/ingress-pin-check.sh --print-tag` prints, and it exits non-zero with no output when Traefik is absent:

    ```sh
    docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-ingress:X.Y.Z --format '{{ json .SBOM }}' \
      | jq -e -r '[.. | .packages? // empty | .[] | .externalRefs[]? | .referenceLocator | select(startswith("pkg:golang/github.com/traefik/traefik/v3@"))] | unique | .[]'
    ```

  - The keepalived image SBOM is the helper's Go modules merged with a hand-written entry for keepalived, OpenSSL and musl (`keepalived/image/sbom/keepalived.spdx.json.in`, filled by the build and shipped in the image at `/usr/share/tierplan/sbom/keepalived.spdx.json`). The filter below must print `pkg:generic/keepalived@2.4.3` and exits non-zero with no output when the entry is absent. Its licence is GPL-2.0-or-later, and the notice and `/licenses/keepalived/COPYING` in the image say so too:

    ```sh
    docker buildx imagetools inspect ghcr.io/ianglennon/tierplan-keepalived:X.Y.Z --format '{{ json .SBOM }}' \
      | jq -e -r '[.. | .packages? // empty | .[] | .externalRefs[]? | .referenceLocator | select(startswith("pkg:generic/keepalived@"))] | unique | .[]'
    ```

    The merge of that entry into the image attestation by the build is not proven until a tagged release has run it. If the filter finds nothing in the first release that ships this image, do not loosen the filter and do not sign the release off: the entry must be in the attestation (the file inside the image is only a second copy), so fix the merge first.

- [ ] **Confirm the image tags.** `X.Y.Z` and `X.Y` exist (a pre-release gets only `X.Y.Z`), both for `linux/amd64` and `linux/arm64`, and **there is no `:latest` or other floating catch-all tag** while TierPlan is 0.x. `docker buildx imagetools inspect ghcr.io/ianglennon/tierplan:latest` must fail.
- [ ] Pull the released image on a clean machine and run the README's install command. It must be healthy, show the setup wizard, and report the right version in Settings > About.
- [ ] **Update the README's image tag from `:edge` to `:X.Y`** (and the upgrade example), in the same release, so the documented command pins the line it was written for.
- [ ] The GitHub release notes group changes by Conventional Commit type; read them and add anything a user must do when upgrading (a breaking change, a new setting, a migration).
