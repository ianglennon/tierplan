// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Package ingress holds the pure rules of TierPlan's ingress model: the types of a domain, a claim
// and a route, the rules for hostnames and domain suffixes, and Check, the one function that decides
// whether a stack may hold the hostnames its labels declare.
//
// The package is pure. The database lives in the subpackage ingressstore, which reads the world and
// runs Check inside its write transaction. This package must not import stacks, store, vault,
// audit, api, server, stackops or acme, or any other package of this module, so internal/stacks can
// import it without a cycle. TestIngressPackageImportsNoInternalPackage pins that.
package ingress
