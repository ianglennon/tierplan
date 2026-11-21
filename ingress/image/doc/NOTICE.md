# Third-party software in the tierplan-ingress image

The `tierplan-ingress` image contains one third-party program, Traefik.

| | |
|---|---|
| Program | Traefik v3.7.19 |
| Source | https://github.com/traefik/traefik/tree/v3.7.19 |
| Commit | 7dafd6dba371bd1aee70c443c2975c229c969711 |
| Licence | MIT, the text is in `LICENSE.md` beside this file |

Traefik is a separate program copied into the image as its own binary. It is not linked into TierPlan code, and TierPlan does not modify it.

## Reviewed exceptions in Traefik's dependency tree

TierPlan's licence gate (`make licences`, `scripts/licences-check.sh`) checks every Go package in Traefik's build against an allowlist on every change. Three reviewed exceptions were needed for Traefik v3.7.19, recorded in `licence-exceptions.txt`:

- `github.com/nrdcg/oci-go-sdk` is dual-licensed under UPL-1.0 or Apache-2.0. Either may be chosen, and TierPlan takes Apache-2.0.
- `github.com/nrdcg/vegadns` has a LICENSE file that holds only the Apache-2.0 notice, so the scanner cannot classify it. It is Apache-2.0.
- `github.com/traefik/traefik/dynamic/ext` is a nested module inside Traefik's own repository with no LICENSE file of its own. It is covered by the repository's MIT licence.

No dependency under the BSL, SSPL, GPL-2.0-only or a proprietary licence is accepted.

## Dependency licences

The licence texts of Traefik's own dependencies are not shipped in this image. To read them, use the image's SBOM attestation, which lists every Go module in Traefik's build info and in the supervisor, and then each module's source repository at the listed version.
