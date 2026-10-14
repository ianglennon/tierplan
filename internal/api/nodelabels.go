// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"github.com/ianglennon/bayplan/internal/dockerx"
)

// The bounds the agent puts on a node's labels. They are counted in UTF-8 bytes, and the
// server's must not be looser than the agent's (plan 06-15), so a label the server accepts is
// one the agent forwards.
const (
	maxNodeLabels     = 64
	maxNodeLabelKey   = 256
	maxNodeLabelValue = 4096

	problemLabelReserved = "label_reserved"
	problemLabelInvalid  = "label_invalid"
)

// NodeLabelImpactRow is one placement constraint of one service that names a label the change
// touches.
type NodeLabelImpactRow struct {
	ServiceID  string `json:"serviceId" doc:"The service the constraint belongs to"`
	Service    string `json:"service" doc:"The service's name"`
	Constraint string `json:"constraint" doc:"The constraint as the service's spec writes it, such as node.labels.disk==ssd"`
	Before     bool   `json:"before" doc:"True when the node meets the constraint with the labels it has now"`
	After      bool   `json:"after" doc:"True when the node would meet the constraint with the labels asked for"`
}

// NodeLabelImpactBody is what a change of one node's labels will do to placement.
type NodeLabelImpactBody struct {
	Version         uint64               `json:"version" doc:"The node's object version as read for this judgement: the version the save must send back"`
	Rows            []NodeLabelImpactRow `json:"rows" doc:"Each placement constraint of a service that names a label the change adds, changes or removes, by service name then constraint. Never null"`
	NoLongerMatches int                  `json:"noLongerMatches" doc:"How many services have a constraint the node met and would no longer meet"`
	TasksMayMove    int                  `json:"tasksMayMove" doc:"How many tasks on the node belong to those services, which the swarm may move off it"`
	Added           int                  `json:"added" doc:"Labels the change adds"`
	Changed         int                  `json:"changed" doc:"Labels whose value the change changes"`
	Removed         int                  `json:"removed" doc:"Labels the change removes"`
}

// labelImpactInput is everything nodeLabelImpact judges.
type labelImpactInput struct {
	// Node is the node with the labels it has now.
	Node dockerx.NodeRow
	// Version is the node's object version at the read.
	Version uint64
	// Labels are the labels asked for, which replace the node's.
	Labels map[string]string
	// Services are the swarm's services.
	Services []dockerx.ServiceRow
	// Tasks are the node's tasks whose desired state is running.
	Tasks []dockerx.TaskRow
}

// reservedLabel reports whether a label key is one Docker keeps for itself.
func reservedLabel(key string) bool { return false }

// labelChanges counts the labels after adds, changes the value of and drops from before. Keys
// are compared byte for byte.
func labelChanges(before, after map[string]string) (added, changed, removed int) { return 0, 0, 0 }

// nodeLabelImpact judges a change of one node's labels against every service's placement
// constraints.
func nodeLabelImpact(in labelImpactInput) NodeLabelImpactBody { return NodeLabelImpactBody{} }
