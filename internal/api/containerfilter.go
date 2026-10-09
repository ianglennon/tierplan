// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/ianglennon/bayplan/internal/dockerx"
	"github.com/ianglennon/bayplan/internal/envs"
	"github.com/ianglennon/bayplan/internal/httpx"
)

// The labels the container list reads. They are the only labels that reach a
// browser, and only as the project, stack and service fields of a row (T-04-29).
const (
	labelProject = "com.docker.compose.project"
	labelStack   = "com.docker.stack.namespace"
	labelService = "com.docker.swarm.service.name"
	// labelAgent marks a Bayplan agent's own container (install.go labelPair).
	labelAgent = "bayplan.agent"
)

// The values of the status filter. Unhealthy is a health, the others are states.
const (
	statusRunning    = "running"
	statusExited     = "exited"
	statusPaused     = "paused"
	statusRestarting = "restarting"
	statusUnhealthy  = "unhealthy"
)

// containerStatuses are the values status= takes.
var containerStatuses = []string{statusRunning, statusExited, statusPaused, statusRestarting, statusUnhealthy}

// problemInvalidStatus is the 422 for a status value that is not one of the five.
const problemInvalidStatus = "invalid_status"

// StatusCountsBody is how many containers each chip of the list counts. A running
// container that is unhealthy is in both running and unhealthy.
type StatusCountsBody struct {
	Running    int `json:"running" doc:"Containers in state running"`
	Exited     int `json:"exited" doc:"Containers in state exited"`
	Paused     int `json:"paused" doc:"Containers in state paused"`
	Restarting int `json:"restarting" doc:"Containers in state restarting"`
	Unhealthy  int `json:"unhealthy" doc:"Containers whose health is unhealthy, whatever their state"`
}

// parseStatuses reads the comma list of status=. An empty list means no filter and
// an unknown value is a 422 (T-04-31).
func parseStatuses(raw string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		if !slices.Contains(containerStatuses, part) {
			return nil, httpx.Problem(http.StatusUnprocessableEntity, problemInvalidStatus,
				fmt.Sprintf("status %q is not one of %s.", part, strings.Join(containerStatuses, ", ")))
		}
		if !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out, nil
}

// hasStatus says whether the row counts under one status value.
func hasStatus(row envs.ContainerRow, status string) bool {
	if status == statusUnhealthy {
		return row.Health == dockerx.HealthUnhealthy
	}
	return row.State == status
}

// statusCounts counts the rows by status. It is called on the rows after the node
// filter and before the search and the status filter, so the numbers on the chips
// never shrink with what the caller typed.
func statusCounts(rows []envs.ContainerRow) StatusCountsBody {
	var c StatusCountsBody
	for _, r := range rows {
		switch r.State {
		case statusRunning:
			c.Running++
		case statusExited:
			c.Exited++
		case statusPaused:
			c.Paused++
		case statusRestarting:
			c.Restarting++
		}
		if r.Health == dockerx.HealthUnhealthy {
			c.Unhealthy++
		}
	}
	return c
}

// selfKindOf says whether a container is Bayplan's own server ("server"), one of
// its agents ("agent") or neither (""). A label can be set by anyone, so this only
// adds care in the UI; the action endpoint decides ownership again from a fresh
// inspect (T-04-32).
func selfKindOf(isSelf bool, labels map[string]string) string {
	switch {
	case isSelf:
		return "server"
	case labels[labelAgent] == "true":
		return "agent"
	}
	return ""
}

// matchesQuery says whether q (already lower-cased) occurs in the name, the image,
// the compose project, the stack namespace, the service name or any label key or
// value of the row. The match is a case-insensitive substring. An empty q matches
// every row.
func matchesQuery(row envs.ContainerRow, q string) bool {
	if q == "" {
		return true
	}
	has := func(s string) bool { return strings.Contains(strings.ToLower(s), q) }
	if has(row.Name) || has(row.Image) ||
		has(row.Labels[labelProject]) || has(row.Labels[labelStack]) || has(row.Labels[labelService]) {
		return true
	}
	for k, v := range row.Labels {
		if has(k) || has(v) {
			return true
		}
	}
	return false
}

// filterContainers keeps the rows that match q and any of statuses, in their order.
// An empty q and an empty statuses keep everything. It runs before the row cap, so
// the cap applies to what the caller asked for.
func filterContainers(rows []envs.ContainerRow, q string, statuses []string) []envs.ContainerRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" && len(statuses) == 0 {
		return rows
	}
	out := make([]envs.ContainerRow, 0, len(rows))
	for _, r := range rows {
		if !matchesQuery(r, q) {
			continue
		}
		if len(statuses) > 0 && !slices.ContainsFunc(statuses, func(s string) bool { return hasStatus(r, s) }) {
			continue
		}
		out = append(out, r)
	}
	return out
}
