// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

package api

import (
	"strings"

	"github.com/ianglennon/bayplan/internal/envs"
)

// The labels the container list reads. They are the only labels that reach a
// browser, and only as the project, stack and service fields of a row (T-04-29).
const (
	labelProject = "com.docker.compose.project"
	labelStack   = "com.docker.stack.namespace"
	labelService = "com.docker.swarm.service.name"
)

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

// filterContainers keeps the rows that match q, in their order. It runs before the
// row cap, so the cap applies to what the caller asked for.
func filterContainers(rows []envs.ContainerRow, q string) []envs.ContainerRow {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return rows
	}
	out := make([]envs.ContainerRow, 0, len(rows))
	for _, r := range rows {
		if matchesQuery(r, q) {
			out = append(out, r)
		}
	}
	return out
}
