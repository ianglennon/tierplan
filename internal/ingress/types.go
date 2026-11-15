// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package ingress

import "time"

// The labels the ingress model reads from a compose file. LabelPrefix starts every label a stack
// uses to declare a route; LabelComponent marks a container that is part of TierPlan's own ingress
// and is never declared by a stack.
const (
	LabelPrefix      = "tierplan.ingress."
	LabelComponent   = "tierplan.component"
	ComponentIngress = "ingress"
)

// ProviderCloudflare is the one DNS provider a domain can be validated through today.
const ProviderCloudflare = "cloudflare"

// Issuer is the ACME directory a domain's certificates come from.
type Issuer string

// The issuers. Staging certificates are not trusted by browsers; they exist to prove the DNS setup
// without spending the production rate limits.
const (
	IssuerStaging    Issuer = "staging"
	IssuerProduction Issuer = "production"
)

// Valid reports whether i is one of the issuers.
func (i Issuer) Valid() bool { return i == IssuerStaging || i == IssuerProduction }

// Scheme is how the proxy speaks to a route's backend.
type Scheme string

// The backend schemes. HTTPS exists for a backend that only serves TLS.
const (
	SchemeHTTP  Scheme = "http"
	SchemeHTTPS Scheme = "https"
)

// OwnerKind says who holds a hostname.
type OwnerKind string

// The owners. A stack holds the hostnames its labels declare. The system holds one TierPlan itself
// serves; no stack can claim or release that one.
const (
	OwnerStack  OwnerKind = "stack"
	OwnerSystem OwnerKind = "system"
)

// Domain is a suffix an environment may serve hostnames under, with what is needed to validate
// them: the issuer and the DNS provider. The provider token is sealed in the row and never leaves
// the store in this type; HasToken only says whether one is saved.
type Domain struct {
	ID             string
	EnvID          string
	Suffix         string
	Issuer         Issuer
	DNSProvider    string
	HasToken       bool
	TokenUpdatedAt time.Time
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Claim is the ownership of one hostname. There is at most one per hostname in the whole install:
// the table's primary key is what makes "one owner per hostname" true.
type Claim struct {
	EnvID     string
	Hostname  string
	DomainID  string
	OwnerKind OwnerKind
	// StackID is the owning stack, "" for a system claim.
	StackID   string
	ClaimedBy string
	ClaimedAt time.Time
}

// ClaimInfo is a Claim with the names a message about it needs.
type ClaimInfo struct {
	Claim
	// EnvName is the name of the environment that holds the claim.
	EnvName string
	// StackName is the name of the claiming stack, "" for a system claim.
	StackName string
}

// Route is one rule of a claimed hostname: requests for Hostname under PathPrefix go to Service on
// Port. Service is the name the compose file uses.
type Route struct {
	ID         string
	EnvID      string
	Hostname   string
	Name       string
	PathPrefix string
	Service    string
	Port       int
	Scheme     Scheme
	// Strip says the proxy removes PathPrefix before it forwards the request.
	Strip     bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Declaration is a route as a stack's labels state it, before anything is stored.
type Declaration struct {
	// Service is the compose service that carries the labels.
	Service string
	// Name is the route's name in the label key, "" for the short form.
	Name string
	Host string
	// Label is the label key that declared the host, so a problem can point at it.
	Label      string
	PathPrefix string
	Port       int
	Scheme     Scheme
	Strip      bool
}

// Severity says how a Problem stops a deploy: an error blocks it, a warning only informs.
type Severity string

// The severities, with the same values internal/stacks uses, so a Problem converts by value.
const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// The codes of the problems this package reports. A code is stable: the editor and the API tests
// key on it, the message is for a person. There is no code for a switched-off ingress: the switch is
// not read here.
const (
	CodeLabelUnknown     = "ingress_label_unknown"
	CodeRouteNameInvalid = "ingress_route_name_invalid"
	CodeHostMissing      = "ingress_host_missing"
	CodeHostInvalid      = "ingress_host_invalid"
	CodePortMissing      = "ingress_port_missing"
	CodePortInvalid      = "ingress_port_invalid"
	CodePathInvalid      = "ingress_path_invalid"
	CodeSchemeInvalid    = "ingress_scheme_invalid"
	CodeStripInvalid     = "ingress_strip_invalid"
	CodeStripRoot        = "ingress_strip_root"
	CodeLabelsWrongPlace = "ingress_labels_wrong_place"
	CodeLabelsIgnored    = "ingress_labels_ignored"
	CodeRouteDuplicate   = "ingress_route_duplicate"
	CodeDomainNotAllowed = "ingress_domain_not_allowed"
	CodeHostReserved     = "ingress_host_reserved"
	CodeHostTaken        = "ingress_host_taken"

	// CodeDomainInvalid and CodeDomainPublicSuffix are the codes of a *NameError, a domain suffix
	// refused at registration.
	CodeDomainInvalid      = "ingress_domain_invalid"
	CodeDomainPublicSuffix = "ingress_domain_public_suffix"
)

// Problem is one thing wrong with a stack's ingress labels.
type Problem struct {
	Severity Severity
	Code     string
	// Service is the compose service the problem is in, "" when it is about the stack as a whole.
	Service string
	// Label is the label key the problem is about, "" when it is about a whole route.
	Label   string
	Message string
}

// HasErrors reports whether any problem blocks the deploy.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == SeverityError {
			return true
		}
	}
	return false
}

// NameError is a hostname or domain suffix refused, with the stable code a caller keys on.
type NameError struct {
	Code    string
	Message string
}

// Error implements error.
func (e *NameError) Error() string { return e.Message }
