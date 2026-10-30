// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// Package registry owns image references, registry credentials, the resolver that
// chooses which credential an image uses, and the registry client. It never reads
// ~/.docker/config.json, a credential helper or a daemon setting: a credential is one
// Coaming stored, chosen by Coaming, and nothing on the host leaks into a pull
// (Phase 5, D-25).
//
// # Using the resolver (Phases 6, 7 and 12)
//
// Resolver.AuthFor(ctx, envID, ref) returns the X-Registry-Auth header for an image and the
// credential it was made from, or "" and nil when no stored credential matches. A pull calls
// it once per job; a service write calls it every time:
//
//   - Call AuthFor on every service create and every service update, and pass the header as
//     EncodedRegistryAuth of the create or update options. A service that is not given a
//     header is pulled anonymously by the nodes.
//   - Pass RegistryAuthFrom: "spec" only when AuthFor returns no credential, so the engine
//     reuses what the service was last written with instead of dropping it.
//   - A service updated without a header keeps the auth it has, so a credential rotated in
//     Coaming reaches a running service only on that service's next update. The engine does
//     not return the auth from service inspect, and a node that has never logged in pulls
//     with it (verified on a real swarm).
//   - Never log, audit, store or return the header, and never put it in an error: it carries
//     the secret. An error from AuthFor names the credential by ID and holds no value.
//   - Record the outcome with Resolver.RecordFailure on an authentication failure and
//     Resolver.ClearFailure on a success, once per job, so the Registries page shows it.
//   - Do not retry with another credential after a failure: one resolution, one attempt.
package registry
