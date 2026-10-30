// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/tunnel"
)

func problemOf(t *testing.T, err error) (status int, ptype string) {
	t.Helper()
	var em *huma.ErrorModel
	if !errors.As(err, &em) {
		t.Fatalf("%v is not a problem", err)
	}
	return em.Status, em.Type
}

// A config list carries every config's content from the manager (RESEARCH E17), so a swarm of
// large configs can pass the tunnel's response cap; that is a 502 that says so (T-06-71).
func TestConfigReadProblemMapsAListTooLargeForTheTunnelToABadGateway(t *testing.T) {
	t.Parallel()
	d := ConfigsDeps{Log: slog.New(slog.DiscardHandler)}
	err := fmt.Errorf("list swarm configs on agent a1: %w", fmt.Errorf("agent a1: %w (more than 67108864 bytes)", tunnel.ErrTooLarge))
	status, ptype := problemOf(t, configReadProblem(d, "env1", err))
	if status != http.StatusBadGateway || ptype != "config_list_too_large" {
		t.Errorf("problem = %d %s, want 502 config_list_too_large", status, ptype)
	}
	if got := configReadProblem(d, "env1", err).Error(); !strings.Contains(got, "too many or too large") {
		t.Errorf("detail = %q, want it to say the configs are too many or too large to list", got)
	}
	status, ptype = problemOf(t, configReadProblem(d, "env1", fmt.Errorf("inspect config: %w", dockerx.ErrObjectNotFound)))
	if status != http.StatusNotFound || ptype != "config_not_found" {
		t.Errorf("a missing config = %d %s, want 404 config_not_found", status, ptype)
	}
}
