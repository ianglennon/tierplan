// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ianglennon/tierplan/internal/httpx"
	"github.com/ianglennon/tierplan/internal/ingress"
)

// problemIngressOwned is the type of the 409 a write to an object TierPlan's ingress owns gets (D-29): the
// object is changed by switching ingress on or off, never by hand. Logs and inspect stay open.
const problemIngressOwned = "ingress_owned"

// ingressOwnedMessage is the sentence every ingress_owned refusal carries.
const ingressOwnedMessage = "This is managed by TierPlan ingress. Switch ingress off to remove it. Nothing was changed."

// ingressNameMessage is the refusal of a start, restart or connect that would put a container on TierPlan's ingress
// network, by its name or by a network named with the start of its ID, under a name a stack deploy does not give
// (T-25-47): the proxy reaches each route by name there, so the name could take another stack's route.
const ingressNameMessage = "This would put the container on TierPlan's ingress network under a name of its own, where it could take another stack's route. Disconnect it from that network first. Nothing was changed."

// errIngressShadow is a network create refused on its engine because the name is the start of the ingress network's ID.
var errIngressShadow = errors.New("api: the network name is the start of the ingress network's ID")

// ingressShadowMessage is the refusal of a connect to, or a create of, a network whose name is the start of the ingress
// network's ID: once no network carries that name, the engine resolves it to the ingress network (T-25-47).
const ingressShadowMessage = "This network's name is the start of the ID of TierPlan's ingress network, so a container joined to it could end up on the ingress network. Use another name. Nothing was changed."

// problemIngressComponentLabel is the type of the 422 a create that sets TierPlan's component label gets (D-41).
const problemIngressComponentLabel = "ingress_component_label"

// ingressOwnedError is the 409 ingress_owned answer. A caller decides to refuse from a fresh inspect of the
// object: the component label and the reserved name together (ingress.OwnsService and its kin), never the
// label alone, so a user's own object that carries the label is never locked.
func ingressOwnedError() error {
	return httpx.Problem(http.StatusConflict, problemIngressOwned, ingressOwnedMessage)
}

// componentLabelError is the 422 ingress_component_label answer to a create that sets the label.
func componentLabelError() error {
	return httpx.Problem(http.StatusUnprocessableEntity, problemIngressComponentLabel,
		"tierplan.component is TierPlan's own label for its ingress objects; remove it.")
}

// hasComponentLabel reports whether labels set TierPlan's component label, whatever its value: a create may not
// set it at all, so nothing can pose as one of TierPlan's ingress objects.
func hasComponentLabel(labels map[string]string) bool {
	_, ok := labels[ingress.LabelComponent]
	return ok
}

// specSetsComponentLabel reports whether a service spec, as the create routes receive it, sets the component
// label on the service or on its container, and the name the spec asks for. A spec that does not decode is not
// judged here: the strict decode of the create refuses it with its own words.
func specSetsComponentLabel(raw []byte) (name string, sets bool) {
	var spec struct {
		Name         string
		Labels       map[string]string
		TaskTemplate struct {
			ContainerSpec struct{ Labels map[string]string }
		}
	}
	if json.Unmarshal(raw, &spec) != nil {
		return "", false
	}
	return spec.Name, hasComponentLabel(spec.Labels) || hasComponentLabel(spec.TaskTemplate.ContainerSpec.Labels)
}
