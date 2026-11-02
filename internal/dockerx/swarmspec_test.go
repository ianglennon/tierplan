// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

package dockerx_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"

	"github.com/ianglennon/tierplan/internal/dockerx"
	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

// The swarm settings read and the task history limit write (plan 11-11, JAN-03, D-17). The fake daemon
// reports the cluster in /info and takes POST /swarm/update.

// settingsSpec is a swarm spec whose every setting but the limit has a value that is not the zero
// value, so an update that dropped one differs from it.
func settingsSpec() swarm.Spec {
	keep := uint64(2)
	return swarm.Spec{
		Annotations:  swarm.Annotations{Name: "default", Labels: map[string]string{"env": "prod"}},
		Raft:         swarm.RaftConfig{SnapshotInterval: 10000, KeepOldSnapshots: &keep, ElectionTick: 10, HeartbeatTick: 1},
		Dispatcher:   swarm.DispatcherConfig{HeartbeatPeriod: 5 * time.Second},
		CAConfig:     swarm.CAConfig{NodeCertExpiry: 90 * 24 * time.Hour},
		TaskDefaults: swarm.TaskDefaults{LogDriver: &swarm.Driver{Name: "json-file", Options: map[string]string{"max-size": "10m"}}},
	}
}

func swarmManager(t *testing.T, tune func(*dockertest.FakeConfig)) (*dockerx.Client, *dockertest.Fake) {
	t.Helper()
	cfg := dockertest.FakeConfig{
		SwarmState: "active", SwarmControl: true, SwarmClusterID: "cluster-1", SwarmVersion: 7,
		HistoryLimit: 5, SwarmSpec: settingsSpec(),
	}
	if tune != nil {
		tune(&cfg)
	}
	fake := dockertest.NewFake(t, cfg)
	return newClient(t, fake), fake
}

func TestSwarmSettingsReadTheRawLimitAndTheVersion(t *testing.T) {
	for name, tc := range map[string]struct {
		limit int
		want  int64
	}{
		"a limit the cluster set":        {limit: 12, want: 12},
		"the limit that keeps every one": {limit: -1, want: -1},
		"a limit never set":              {limit: 0, want: dockerx.DefaultTaskHistoryLimit},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := swarmManager(t, func(cfg *dockertest.FakeConfig) { cfg.HistoryLimit = tc.limit })
			set, err := c.SwarmSettings(context.Background())
			if err != nil {
				t.Fatalf("SwarmSettings: %v", err)
			}
			if set.TaskHistoryLimit != tc.want || set.Version != 7 || !set.Manager {
				t.Fatalf("settings = %+v, want limit %d, version 7, manager", set, tc.want)
			}
			if set.Spec.Name != "default" || set.Spec.Raft.SnapshotInterval != 10000 {
				t.Errorf("spec = %+v, want the rest of the cluster's spec kept", set.Spec)
			}
		})
	}
}

func TestSwarmSettingsRefuseAnEngineThatIsNotAManager(t *testing.T) {
	for name, tune := range map[string]func(*dockertest.FakeConfig){
		"a worker":              func(cfg *dockertest.FakeConfig) { cfg.SwarmControl = false },
		"an engine in no swarm": func(cfg *dockertest.FakeConfig) { *cfg = dockertest.FakeConfig{} },
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := swarmManager(t, tune)
			if _, err := c.SwarmSettings(context.Background()); !errors.Is(err, dockerx.ErrNotSwarmManager) {
				t.Fatalf("err = %v, want ErrNotSwarmManager", err)
			}
		})
	}
}

// The update carries the version as its only key. The engine's own client would add the three rotate
// keys (as false), which an agent's route refuses, so this is what keeps the call working through
// an agent and keeps a join token or the unlock key from ever being rotated by it.
func TestSetTaskHistoryLimitNoRotate(t *testing.T) {
	c, fake := swarmManager(t, nil)
	set, err := c.SwarmSettings(context.Background())
	if err != nil {
		t.Fatalf("SwarmSettings: %v", err)
	}
	if err := c.SetTaskHistoryLimit(context.Background(), set, 10); err != nil {
		t.Fatalf("SetTaskHistoryLimit: %v", err)
	}
	if got := fake.Writes(); len(got) != 1 || got[0] != "POST /swarm/update?version=7" {
		t.Fatalf("writes = %v, want one POST /swarm/update with only version=7", got)
	}
	ups := fake.SwarmUpdates()
	if len(ups) != 1 || ups[0].RawQuery != "version=7" || strings.Contains(strings.ToLower(ups[0].RawQuery), "rotate") {
		t.Fatalf("updates = %+v, want one whose query is version=7 and nothing else", ups)
	}
	want := settingsSpec()
	ten := int64(10)
	want.Orchestration.TaskHistoryRetentionLimit = &ten
	if !reflect.DeepEqual(ups[0].Spec, want) {
		t.Fatalf("spec sent = %+v, want the cluster's own spec with only the limit set to 10: %+v", ups[0].Spec, want)
	}
	// What the caller read is not changed by the write.
	if lim := set.Spec.Orchestration.TaskHistoryRetentionLimit; lim == nil || *lim != 5 {
		t.Errorf("the settings read now hold the limit %v, want them left at 5", lim)
	}
	after, err := c.SwarmSettings(context.Background())
	if err != nil || after.TaskHistoryLimit != 10 || after.Version != 8 {
		t.Fatalf("after = %+v, %v, want limit 10 at version 8", after, err)
	}
}

// Zero is a limit like any other: it is sent, and read back as zero rather than as Docker's default.
func TestSetTaskHistoryLimitZeroIsSentAndReadBack(t *testing.T) {
	c, fake := swarmManager(t, nil)
	set, err := c.SwarmSettings(context.Background())
	if err != nil {
		t.Fatalf("SwarmSettings: %v", err)
	}
	if err := c.SetTaskHistoryLimit(context.Background(), set, 0); err != nil {
		t.Fatalf("SetTaskHistoryLimit: %v", err)
	}
	if ups := fake.SwarmUpdates(); len(ups) != 1 || ups[0].Spec.Orchestration.TaskHistoryRetentionLimit == nil || *ups[0].Spec.Orchestration.TaskHistoryRetentionLimit != 0 {
		t.Fatalf("updates = %+v, want one carrying the limit 0, not leaving it out", ups)
	}
	after, err := c.SwarmSettings(context.Background())
	if err != nil || after.TaskHistoryLimit != 0 {
		t.Fatalf("after = %+v, %v, want limit 0", after, err)
	}
}

// A swarm whose version moved after it was read is ErrSwarmChanged, the update is sent once, and
// nothing is changed.
func TestSetTaskHistoryLimitSwarmChanged(t *testing.T) {
	c, fake := swarmManager(t, func(cfg *dockertest.FakeConfig) { cfg.SwarmChangedUnderUpdate = true })
	set, err := c.SwarmSettings(context.Background())
	if err != nil {
		t.Fatalf("SwarmSettings: %v", err)
	}
	if err := c.SetTaskHistoryLimit(context.Background(), set, 10); !errors.Is(err, dockerx.ErrSwarmChanged) {
		t.Fatalf("err = %v, want ErrSwarmChanged", err)
	}
	if got := fake.Writes(); len(got) != 1 {
		t.Fatalf("writes = %v, want the one update and no retry", got)
	}
	if ups := fake.SwarmUpdates(); len(ups) != 0 {
		t.Fatalf("accepted updates = %+v, want none", ups)
	}
}

// An agent's refusal keeps its words, which is how the server tells an older agent from a fault.
func TestSetTaskHistoryLimitKeepsAnAgentsRefusal(t *testing.T) {
	c, _ := swarmManager(t, func(cfg *dockertest.FakeConfig) { cfg.SwarmUpdateDeny = true })
	set, err := c.SwarmSettings(context.Background())
	if err != nil {
		t.Fatalf("SwarmSettings: %v", err)
	}
	err = c.SetTaskHistoryLimit(context.Background(), set, 10)
	if err == nil || !strings.Contains(err.Error(), "agent_policy_denied: POST /") || errors.Is(err, dockerx.ErrSwarmChanged) {
		t.Fatalf("err = %v, want the agent's refusal in its words", err)
	}
}
