// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors
//go:build integration

package dockerx_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/ianglennon/coaming/internal/dockerx"
	"github.com/ianglennon/coaming/internal/dockerx/dockertest"
)

// The sizes the swarm takes (RESEARCH E18): a secret of up to 511,999 bytes and a config of up to
// 1,023,999. One byte more is refused by the manager.
const (
	maxSecretBytes = 511999
	maxConfigBytes = 1023999
)

// TestSwarmObjectsOnARealEngine creates secrets and configs against a real engine (plan 06-37, RES-04, RES-05,
// RES-08): both are created at the largest size the swarm takes and refused one byte over, a name taken is
// ErrNameTaken, the manager refuses to remove an object a service names even when the service has no replicas
// (RESEARCH E16) and lets it go once the service is gone, a missing object is ErrObjectNotFound, and what the
// lists and the inspect of a secret hold never includes a value, while a config's content is read on its own
// route only (RESEARCH E17).
func TestSwarmObjectsOnARealEngine(t *testing.T) {
	f := startFacadeFix(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Secrets: the largest value, one byte over, a taken name.
	value := bytes.Repeat([]byte("s"), maxSecretBytes)
	secretID, err := f.cli.SecretCreate(ctx, dockerx.SecretSpec{Name: "obj-secret", Labels: map[string]string{"owner": "tests"}, Data: value})
	if err != nil {
		t.Fatalf("SecretCreate of %d bytes: %v", maxSecretBytes, err)
	}
	if _, err := f.cli.SecretCreate(ctx, dockerx.SecretSpec{Name: "obj-secret-big", Data: bytes.Repeat([]byte("s"), maxSecretBytes+1)}); err == nil {
		t.Errorf("SecretCreate of %d bytes succeeded: the swarm's limit is %d", maxSecretBytes+1, maxSecretBytes)
	} else if errors.Is(err, dockerx.ErrNameTaken) {
		t.Errorf("SecretCreate one byte over the limit = %v, want the manager's own refusal, not a name clash", err)
	}
	if _, err := f.cli.SecretCreate(ctx, dockerx.SecretSpec{Name: "obj-secret", Data: []byte("other")}); !errors.Is(err, dockerx.ErrNameTaken) {
		t.Errorf("SecretCreate of a name that is taken = %v, want ErrNameTaken", err)
	}

	// Configs: the same, with the content read back whole and the templating driver kept.
	content := bytes.Repeat([]byte("c"), maxConfigBytes)
	configID, err := f.cli.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "obj-config", Labels: map[string]string{"owner": "tests"}, Data: content, Templating: "golang"})
	if err != nil {
		t.Fatalf("ConfigCreate of %d bytes: %v", maxConfigBytes, err)
	}
	if _, err := f.cli.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "obj-config-big", Data: bytes.Repeat([]byte("c"), maxConfigBytes+1)}); err == nil {
		t.Errorf("ConfigCreate of %d bytes succeeded: the swarm's limit is %d", maxConfigBytes+1, maxConfigBytes)
	} else if errors.Is(err, dockerx.ErrNameTaken) {
		t.Errorf("ConfigCreate one byte over the limit = %v, want the manager's own refusal, not a name clash", err)
	}
	if _, err := f.cli.ConfigCreate(ctx, dockerx.ConfigSpec{Name: "obj-config", Data: []byte("other")}); !errors.Is(err, dockerx.ErrNameTaken) {
		t.Errorf("ConfigCreate of a name that is taken = %v, want ErrNameTaken", err)
	}

	// Lists: rows with the name, the labels and the driver, and no content. The engine itself sends the content of
	// every config with its list (RESEARCH E17), which is why the facade drops it.
	secrets, err := f.cli.SecretList(ctx)
	if err != nil || len(secrets) != 1 || secrets[0].ID != secretID || secrets[0].Name != "obj-secret" || secrets[0].Labels["owner"] != "tests" {
		t.Fatalf("SecretList = %+v, %v, want the one secret", secrets, err)
	}
	rawList, err := f.raw.ConfigList(ctx, client.ConfigListOptions{})
	if err != nil || len(rawList.Items) != 1 || len(rawList.Items[0].Spec.Data) != maxConfigBytes {
		t.Fatalf("the engine's own config list holds %d items, %v: the premise of the facade's drop (E17) no longer holds", len(rawList.Items), err)
	}
	configs, err := f.cli.ConfigList(ctx)
	if err != nil || len(configs) != 1 || configs[0].ID != configID || configs[0].Name != "obj-config" || configs[0].Templating != "golang" || configs[0].Labels["owner"] != "tests" {
		t.Fatalf("ConfigList = %+v, %v, want the one config with its driver", configs, err)
	}
	row, rawSecret, err := f.cli.SecretInspect(ctx, "obj-secret")
	if err != nil || row.ID != secretID {
		t.Fatalf("SecretInspect = %+v, %v", row, err)
	}
	if bytes.Contains(rawSecret, value[:64]) || bytes.Contains(rawSecret, []byte(base64.StdEncoding.EncodeToString(value)[:64])) {
		t.Error("the raw document of a secret holds its value")
	}
	detail, _, err := f.cli.ConfigInspect(ctx, "obj-config")
	if err != nil || !bytes.Equal(detail.Data, content) || detail.Templating != "golang" {
		t.Fatalf("ConfigInspect = %d bytes, templating %q, %v, want the content back whole", len(detail.Data), detail.Templating, err)
	}

	// A service with no replicas still names the secret and the config, and the manager still refuses to remove them.
	zero := uint64(0)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: "obj-user"},
		TaskTemplate: swarm.TaskSpec{ContainerSpec: &swarm.ContainerSpec{
			Image:   dockertest.ServiceTestImage,
			Command: []string{"sleep", "3600"},
			Secrets: []*swarm.SecretReference{{SecretID: secretID, SecretName: "obj-secret", File: &swarm.SecretReferenceFileTarget{Name: "obj-secret", UID: "0", GID: "0", Mode: 0o444}}},
			Configs: []*swarm.ConfigReference{{ConfigID: configID, ConfigName: "obj-config", File: &swarm.ConfigReferenceFileTarget{Name: "/obj-config", UID: "0", GID: "0", Mode: 0o444}}},
		}},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &zero}},
	}
	serviceID, _, err := f.cli.ServiceCreate(ctx, specOf(t, spec), "")
	if err != nil {
		t.Fatalf("ServiceCreate naming the secret and the config at 0 replicas: %v", err)
	}
	if err := f.cli.SecretRemove(ctx, secretID); !errors.Is(err, dockerx.ErrObjectInUse) {
		t.Errorf("SecretRemove of a secret a service names = %v, want ErrObjectInUse (RESEARCH E16)", err)
	}
	if err := f.cli.ConfigRemove(ctx, configID); !errors.Is(err, dockerx.ErrObjectInUse) {
		t.Errorf("ConfigRemove of a config a service names = %v, want ErrObjectInUse (RESEARCH E16)", err)
	}
	if err := f.cli.ServiceRemove(ctx, serviceID); err != nil {
		t.Fatalf("ServiceRemove: %v", err)
	}
	// The remove of the service reaches the objects' in-use check at once: nothing is left to wait on, but the
	// manager is the judge, so the removals are polled rather than assumed.
	dockertest.Wait(t, time.Minute, "the secret and the config to be removable once the service is gone", func() (bool, string) {
		var seen []string
		if err := f.cli.SecretRemove(ctx, secretID); err != nil && !errors.Is(err, dockerx.ErrObjectNotFound) {
			seen = append(seen, err.Error())
		}
		if err := f.cli.ConfigRemove(ctx, configID); err != nil && !errors.Is(err, dockerx.ErrObjectNotFound) {
			seen = append(seen, err.Error())
		}
		return len(seen) == 0, strings.Join(seen, "; ")
	})
	if err := f.cli.SecretRemove(ctx, secretID); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("SecretRemove of a secret that is gone = %v, want ErrObjectNotFound", err)
	}
	if err := f.cli.ConfigRemove(ctx, configID); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("ConfigRemove of a config that is gone = %v, want ErrObjectNotFound", err)
	}
	if _, _, err := f.cli.SecretInspect(ctx, "obj-secret"); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("SecretInspect of a secret that is gone = %v, want ErrObjectNotFound", err)
	}
	if _, _, err := f.cli.ConfigInspect(ctx, "obj-config"); !errors.Is(err, dockerx.ErrObjectNotFound) {
		t.Errorf("ConfigInspect of a config that is gone = %v, want ErrObjectNotFound", err)
	}
	if left, err := f.cli.SecretList(ctx); err != nil || len(left) != 0 {
		t.Errorf("SecretList after the removals = %+v, %v, want none", left, err)
	}
	if left, err := f.cli.ConfigList(ctx); err != nil || len(left) != 0 {
		t.Errorf("ConfigList after the removals = %+v, %v, want none", slices.Clone(left), err)
	}
}
