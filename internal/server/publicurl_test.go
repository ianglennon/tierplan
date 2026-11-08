package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"github.com/ianglennon/tierplan/internal/config"
)

func loadConfig(env map[string]string) (config.Config, error) {
	fl := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fl)
	return config.Load(fl, func(k string) string { return env[k] }, os.ReadFile)
}

func TestAnHTTPPublicURLVariableStopsStartup(t *testing.T) {
	for _, name := range []string{"TIERPLAN_PUBLIC_URL", "TIERPLAN_AGENT_URL"} {
		_, err := loadConfig(map[string]string{name: "http://tierplan.test"})
		if err == nil {
			t.Fatalf("%s=http://tierplan.test started", name)
		}
		if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "serves HTTPS only") {
			t.Errorf("%s: error = %v, want the variable name and the HTTPS-only reason", name, err)
		}
	}
}

func TestAnHTTPSPublicURLVariableStarts(t *testing.T) {
	cfg := testConfig(t, map[string]string{
		"TIERPLAN_DATA_DIR":   t.TempDir() + "/data",
		"TIERPLAN_PUBLIC_URL": "https://tierplan.test",
		"TIERPLAN_AGENT_URL":  "https://agents.tierplan.test",
	})
	if cfg.PublicURL != "https://tierplan.test" || cfg.AgentURL != "https://agents.tierplan.test" {
		t.Fatalf("urls = %q %q", cfg.PublicURL, cfg.AgentURL)
	}
	srv, err := Build(context.Background(), cfg, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("Build with https URLs: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
}
