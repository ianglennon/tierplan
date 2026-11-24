// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//go:build integration

package dockerx_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/ianglennon/tierplan/internal/dockerx/dockertest"
)

const (
	// s3StatsBound bounds the wait for keepalived to write its statistics after SIGUSR2, and for the counter to rise once a
	// foreign router is on the segment (the foreign adverts come once a second).
	s3StatsBound = 30 * time.Second
	// s3AuthWindow is the time_window the rig's configuration names (A-04: 30 s unless the run argues otherwise).
	s3AuthWindow = 30
)

var (
	// s3AuthBlock finds the counters of the instance's `Authentication Extension Errors` block in the statistics file.
	s3AuthBlock   = regexp.MustCompile(`(?m)^\s*(Missing|Malformed|Unknown Key|Invalid HMAC|Stale|Replay):\s*(\d+)\s*$`)
	s3InvalidHMAC = regexp.MustCompile(`invalid authentication HMAC from (\S+)`)
	s3WindowText  = regexp.MustCompile(`(?i)window[ =:]+(\d+)`)
)

// authCounters sends SIGUSR2 to the keepalived container on n, reads /tmp/keepalived.stats and returns the counters under
// `Authentication Extension Errors` with the file's text. The file is rewritten on each signal, so a caller that needs a fresh
// dump polls. `docker cp` cannot read it (/tmp is a tmpfs, which the engine's archive API does not see: "Could not find the
// file"), and the image has no shell or cat, so the node reads it through the container's root in /proc.
func authCounters(ctx context.Context, n *dockertest.Node, id string) (map[string]int, string, error) {
	if out, err := n.Exec(ctx, "docker", "kill", "-s", "USR2", id); err != nil {
		return nil, "", fmt.Errorf("send SIGUSR2: %w: %s", err, out)
	}
	b, err := n.Sh(ctx, "cat /proc/$(docker inspect -f '{{.State.Pid}}' "+id+")/root/tmp/keepalived.stats")
	if err != nil {
		return nil, "", fmt.Errorf("read the statistics file: %w", err)
	}
	text := b
	i := strings.Index(text, "Authentication Extension Errors")
	if i < 0 {
		return nil, text, fmt.Errorf("no Authentication Extension Errors block yet")
	}
	counters := map[string]int{}
	for _, m := range s3AuthBlock.FindAllStringSubmatch(text[i:], -1) {
		n, _ := strconv.Atoi(m[2])
		if _, seen := counters[m[1]]; !seen {
			counters[m[1]] = n
		}
	}
	if _, ok := counters["Invalid HMAC"]; !ok {
		return nil, text, fmt.Errorf("no Invalid HMAC counter in the block")
	}
	return counters, text, nil
}

// foreignConf is keepalived's configuration for the foreign router: the same router ID, its own key and another address, and
// no check (a check would fail there and keep the router in FAULT, silent).
func foreignConf(vip netip.Prefix, keyPath string, routerID int) string {
	return fmt.Sprintf(`vrrp_instance FOREIGN {
    state BACKUP
    interface eth0
    virtual_router_id %d
    priority 100
    advert_int 1
    auth_hmac {
        key 1 file:%s
        active_key 1
        mode enforce
    }
    virtual_ipaddress {
        %s dev eth0
    }
}
`, routerID, keyPath, vip)
}

// startForeign runs a second keepalived in a plain container on the nodes' bridge: the same router ID as the swarm's, another
// 32-byte key and another address. The configuration and key are put in with CopyToContainer before the start, so it works
// against a remote engine. The container is removed at cleanup. It returns the container ID and the address it was given on
// the bridge, which is what the holder's log should name as the sender.
func (r *s3Rig) startForeign(vip netip.Prefix) (string, netip.Addr) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		r.t.Fatalf("make the foreign key: %v", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct {
		name string
		mode int64
		data string
	}{
		{"etc/keepalived/foreign.key", 0o400, "hex:" + hex.EncodeToString(raw)},
		{"etc/keepalived/keepalived.conf", 0o444, foreignConf(vip, "/etc/keepalived/foreign.key", 51)},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			r.t.Fatalf("tar %s: %v", f.name, err)
		}
		if _, err := tw.Write([]byte(f.data)); err != nil {
			r.t.Fatalf("tar %s: %v", f.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		r.t.Fatalf("close the tar: %v", err)
	}

	res, err := r.host.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "s3-foreign-" + hex.EncodeToString(raw2(4)),
		Config: &container.Config{
			Image:      s3Image,
			Entrypoint: []string{"/usr/sbin/keepalived"},
			Cmd:        []string{"--dont-fork", "--log-console", "--log-detail", "--use-file", "/etc/keepalived/keepalived.conf"},
			Tty:        true,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(r.sw.Network),
			CapDrop:     []string{"ALL"},
			CapAdd:      []string{"NET_ADMIN", "NET_RAW"},
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{r.sw.Network: {}}},
	})
	if err != nil {
		r.t.Fatalf("create the foreign router: %v", err)
	}
	id := res.ID
	r.t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), time.Minute)
		defer ccancel()
		_, _ = r.host.ContainerRemove(cctx, id, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := r.host.CopyToContainer(ctx, id, client.CopyToContainerOptions{DestinationPath: "/", Content: &buf}); err != nil {
		r.t.Fatalf("put the foreign configuration in: %v", err)
	}
	if _, err := r.host.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		r.t.Fatalf("start the foreign router: %v", err)
	}
	insp, err := r.host.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		r.t.Fatalf("inspect the foreign router: %v", err)
	}
	ep := insp.Container.NetworkSettings.Networks[r.sw.Network]
	if ep == nil || !ep.IPAddress.IsValid() {
		r.t.Fatalf("the foreign router has no address on %s: %+v", r.sw.Network, insp.Container.NetworkSettings.Networks)
	}
	return id, ep.IPAddress
}

// foreignLogs is what the foreign router has written (a TTY container, so the stream is not multiplexed).
func (r *s3Rig) foreignLogs(id string) string {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	rc, err := r.host.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "(no logs: " + err.Error() + ")"
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// TestS3ForeignRouterCounters is A-04 and D-06: a keepalived with another key and the same router ID on the nodes' bridge.
// After SIGUSR2 the holder's statistics show the Invalid HMAC counter rising, and the rate-limited log line names the sender's
// address. It also records the auth window (the rig names `anti_replay time` and `time_window 30`; the run shows that
// keepalived accepts them and elects) and whether the foreign router and the swarm holder both became MASTER.
func TestS3ForeignRouterCounters(t *testing.T) {
	r := newS3Swarm(t)
	r.start(s3Opts{timeWindow: s3AuthWindow})
	dockertest.WaitRunningTasks(t, r.sw.Managers[0], r.svcID, len(r.sw.Nodes()), s3Settle)
	holder, _ := r.waitOneHolder(s3FirstHolder, nil)
	ctr := r.taskContainer(holder)
	if ctr == "" {
		t.Fatalf("%s holds the address but has no running keepalived container", holder.Hostname)
	}

	var before map[string]int
	dockertest.Wait(t, s3StatsBound, "the holder's statistics file", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, _, err := authCounters(ctx, holder, ctr)
		before = c
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	})
	if before["Invalid HMAC"] != 0 {
		t.Fatalf("Invalid HMAC is %d before any foreign router: %v", before["Invalid HMAC"], before)
	}

	a := r.vip.Addr().As4()
	a[3] = 249
	foreignID, foreignIP := r.startForeign(netip.PrefixFrom(netip.AddrFrom4(a), r.vip.Bits()))
	t.Logf("the foreign router %s runs at %s with router ID 51 and its own key", foreignID[:12], foreignIP)

	var after map[string]int
	var stats string
	dockertest.Wait(t, s3StatsBound, "the Invalid HMAC counter on "+holder.Hostname+" to rise", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		c, text, err := authCounters(ctx, holder, ctr)
		if err != nil {
			return false, err.Error()
		}
		after, stats = c, text
		return c["Invalid HMAC"] > before["Invalid HMAC"], fmt.Sprintf("counters %v", c)
	})
	t.Logf("counters before %v after %v", before, after)

	// The log line is rate limited (once until the next state change) and is read for the sender only.
	sender := "absent"
	logs := ""
	s3Until(20*time.Second, func() bool {
		logs = r.logs(holder, ctr)
		m := s3InvalidHMAC.FindStringSubmatch(logs)
		if m == nil {
			return false
		}
		if ip, err := netip.ParseAddr(strings.TrimRight(m[1], ",:")); err == nil && ip == foreignIP {
			sender = "ok"
		}
		return true
	})
	t.Logf("S3 foreign-counters: engine=%s invalid_hmac rising sender=%s", r.sw.Engine, sender)

	window := "default"
	if m := s3WindowText.FindStringSubmatch(stats + "\n" + logs); m != nil {
		window = m[1]
	}
	t.Logf("S3 time-window: engine=%s seconds=%s configured=%d", r.sw.Engine, window, s3AuthWindow)

	// Both routers reject the other's adverts, so each is MASTER of its own address: the split brain the text of D-20 warns of.
	foreignMaster := s3Until(20*time.Second, func() bool {
		return strings.Contains(r.foreignLogs(foreignID), "Entering MASTER STATE")
	})
	switch {
	case foreignMaster && r.has(holder, ""):
		t.Logf("S3 foreign-split: engine=%s both-master", r.sw.Engine)
	case !r.has(holder, ""):
		t.Logf("S3 foreign-split: engine=%s holder-moved foreign-master=%v", r.sw.Engine, foreignMaster)
	default:
		t.Logf("S3 foreign-split: engine=%s none foreign-master=%v", r.sw.Engine, foreignMaster)
	}
}
