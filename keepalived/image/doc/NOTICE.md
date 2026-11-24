# Third-party software in the tierplan-keepalived image

The `tierplan-keepalived` image contains one third-party program, keepalived.

| | |
|---|---|
| Program | keepalived 2.4.3 |
| Source | https://www.keepalived.org/software/keepalived-2.4.3.tar.gz |
| SHA-256 of the source | a0faef8e401c143487b131b526df7541c1e33d9b8814642fa9dfe8bb250a9632 |
| Licence | GPL-2.0-or-later, the text is in `/licenses/keepalived/COPYING` |

keepalived is built from that unmodified tarball. The build checks the tarball against the SHA-256 above before it compiles anything. It is built with LVS, iptables, nftables, systemd and track_process support turned off, and OpenSSL's libcrypto is linked into the binary statically for the authenticated adverts (`auth_hmac`).

The unmodified source tarball is attached to every TierPlan release, so the source of the binary in this image is available beside the image itself. The same tarball is published by the keepalived project at the address above.

The `tierplan-ingress` program in this image (`/usr/local/bin/tierplan-ingress`) is TierPlan's own helper and is licensed AGPL-3.0-or-later. It is a separate program that starts keepalived as a child process and talks to it through a configuration file, a log and signals; it is not linked with keepalived.

The image's SBOM entry for keepalived and OpenSSL is `/usr/share/tierplan/sbom/keepalived.spdx.json`. The Go modules of the helper are listed by the image's SBOM attestation.
