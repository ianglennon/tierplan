// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingressbundle

// The names of the environment variables the ingress supervisor reads. They are a contract between two
// modules: the supervisor (ingress/internal/config) reads them and the server's deployer writes them into
// the proxy service, so both import these constants and no second copy of the strings exists (D-47).
const (
	// EnvURL is the full address of the server's ingress channel, for example
	// wss://tierplan.example:7480/ingress/connect (D-33).
	EnvURL = "TIERPLAN_INGRESS_URL"
	// EnvPin is the pin of the server's certificate, or several separated by
	// commas, as "sha256/" and a base64 SPKI hash (D-32). Without it the server
	// must present a certificate the system trusts.
	EnvPin = "TIERPLAN_INGRESS_PIN"
	// EnvJoinTokenFile is the path of a Docker secret holding the join token. It
	// wins over EnvJoinToken.
	EnvJoinTokenFile = "TIERPLAN_INGRESS_JOIN_TOKEN_FILE" //nolint:gosec // the name of a variable, not a credential
	// EnvJoinToken is the join token itself.
	EnvJoinToken = "TIERPLAN_INGRESS_JOIN_TOKEN" //nolint:gosec // the name of a variable, not a credential
	// EnvNodeID is the swarm node ID of the host the supervisor runs on. A
	// standalone node has none, so it is optional (D-22).
	EnvNodeID = "TIERPLAN_INGRESS_NODE_ID"
	// EnvAccessLog set to 1 switches Traefik's JSON access log on stdout on; set by TierPlan from the
	// environment's access-log setting (D-31). Unset or empty leaves it off.
	EnvAccessLog = "TIERPLAN_INGRESS_ACCESS_LOG"
)
