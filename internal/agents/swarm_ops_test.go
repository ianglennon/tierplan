// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors
//go:build swarm

package agents_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ianglennon/bayplan/internal/dockerx/dockertest"
	"github.com/ianglennon/bayplan/internal/server/servertest"
)

// extraAgent runs a second standalone agent on a node, from an install command,
// with its own container name and state volume so it neither collides with the
// swarm's agent nor inherits its identity. It enrols from scratch with the token
// in the command.
func extraAgent(t *testing.T, n *dockertest.Node, command string) {
	t.Helper()
	cmd := strings.NewReplacer("--name bayplan-agent", "--name extra-agent", "-v bayplan-agent:", "-v extra-agent:").Replace(command)
	sh(t, n, cmd)
}

// agentLog is the log of the extra agent.
func agentLog(t *testing.T, n *dockertest.Node) string {
	t.Helper()
	out, _ := n.Sh(execCtx(t, time.Minute), "docker logs extra-agent 2>&1")
	return out
}

// requireRefusalReason waits for the audit log to hold an agent.enrol_refused entry
// with this reason.
func (f *swarmFix) requireRefusalReason(t *testing.T, reason string) {
	t.Helper()
	f.srv.WaitFor("the audit log to record "+reason, 30*time.Second, func() (bool, string) {
		code, body := f.srv.Do(http.MethodGet, "/api/audit?action=agent.enrol_refused", nil)
		return code == http.StatusOK && strings.Contains(string(body), reason), string(body)
	})
}

// sharedRotateToken rotates the join token of a live swarm and runs the generated
// rotate command on a manager, then disables the token and removes everything with
// the manual cleanup text: the commands an admin is shown, run for real (02-09,
// 02-10, 02-14, 02-16).
func sharedRotateToken(t *testing.T, f *swarmFix) {
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	f.install(t)
	f.waitSwarmOnline(t, 4)
	before := f.srv.Agents(f.env.ID)
	oldSecret := f.env.Install.SecretName

	rot := f.srv.RotateToken(f.env.ID)
	if rot.EnvType != "swarm" || !rot.HasAgents || rot.Install.Generation != 2 || rot.Install.Swarm.RotateCommand == "" {
		t.Fatalf("rotate answer = envType %q hasAgents %v generation %d rotate command %q", rot.EnvType, rot.HasAgents, rot.Install.Generation, rot.Install.Swarm.RotateCommand)
	}
	if rot.Install.SecretName == oldSecret {
		t.Fatalf("the rotated install reuses the secret name %s", oldSecret)
	}

	// The old command stops enrolling hosts at once; the enrolled agents are not touched.
	extraAgent(t, worker, f.env.Install.Standalone.Command)
	f.srv.WaitFor("the old token to be refused", 30*time.Second, func() (bool, string) {
		log := agentLog(t, worker)
		return strings.Contains(log, "code=auth_rejected"), log
	})
	// The agent is told one thing for every kind of refusal; the audit knows which.
	f.requireRefusalReason(t, "token_superseded")
	sh(t, worker, "docker rm -f extra-agent; docker volume rm extra-agent")

	// The rotate command, as generated: new secret, service updated, old secret removed.
	t.Logf("rotate on %s:\n%s", mgr.Hostname, quiet(sh(t, mgr, rot.Install.Swarm.RotateCommand)))
	spec := inspectService(t, mgr, agentName)
	if len(spec.TaskTemplate.ContainerSpec.Secrets) != 1 || spec.TaskTemplate.ContainerSpec.Secrets[0].SecretName != rot.Install.SecretName {
		t.Errorf("service secrets = %+v, want only %s", spec.TaskTemplate.ContainerSpec.Secrets, rot.Install.SecretName)
	}
	if secrets := strings.Fields(sh(t, mgr, "docker secret ls --format '{{.Name}}'")); !slices.Equal(secrets, []string{rot.Install.SecretName}) {
		t.Errorf("secrets after the rotation = %v, want only %s", secrets, rot.Install.SecretName)
	}

	// The same four agents are back: no new rows, no re-enrolment.
	f.waitSwarmOnline(t, 4)
	after := f.srv.Agents(f.env.ID)
	if len(after) != len(before) {
		t.Errorf("agent rows after the rotation = %d, want the same %d", len(after), len(before))
	}
	ids := func(as []servertest.Agent) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.ID)
		}
		slices.Sort(out)
		return out
	}
	if !slices.Equal(ids(before), ids(after)) {
		t.Errorf("agent IDs changed across the rotation:\n before %v\n after  %v", ids(before), ids(after))
	}
	var status struct {
		Generation int  `json:"generation"`
		Disabled   bool `json:"disabled"`
	}
	if code, body := f.srv.Do(http.MethodGet, "/api/environments/"+f.env.ID+"/token", nil); code != http.StatusOK || json.Unmarshal(body, &status) != nil || status.Generation != 2 || status.Disabled {
		t.Errorf("GET token = %d %s, want generation 2, not disabled", code, body)
	}

	// Disable the token: a host with the current command is refused, the swarm keeps running.
	if code, body := f.srv.Do(http.MethodDelete, "/api/environments/"+f.env.ID+"/token", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE token = %d %s", code, body)
	}
	extraAgent(t, worker, rot.Install.Standalone.Command)
	f.srv.WaitFor("the disabled token to be refused", 30*time.Second, func() (bool, string) {
		log := agentLog(t, worker)
		return strings.Contains(log, "code=auth_rejected"), log
	})
	f.requireRefusalReason(t, "token_disabled")
	sh(t, worker, "docker rm -f extra-agent; docker volume rm extra-agent")
	if env, _ := f.srv.Environment(f.env.ID); env.Agents == nil || env.Agents.Connected != 4 {
		t.Errorf("after disabling the token, agents = %+v, want all four still connected", env.Agents)
	}
	if got := len(f.srv.Agents(f.env.ID)); got != 4 {
		t.Errorf("agent rows after the refused hosts = %d, want 4", got)
	}

	// The manual cleanup, as shown: the service and the secret on a manager, the
	// volume on every node. Nothing is left behind (the product's own promise).
	lines := strings.Split(strings.TrimSpace(rot.Install.ManualCleanup.Swarm), "\n")
	if len(lines) != 2 {
		t.Fatalf("the swarm cleanup text has %d lines, want 2: %q", len(lines), rot.Install.ManualCleanup.Swarm)
	}
	sh(t, mgr, lines[0])
	for _, n := range f.sw.Nodes() {
		// The volume is in use until the task's container is gone.
		dockertest.Wait(t, 60*time.Second, "the agent container to leave "+n.Hostname, func() (bool, string) {
			out, _ := n.Sh(execCtx(t, time.Minute), "docker ps -aq --filter label=bayplan.agent=true")
			return strings.TrimSpace(out) == "", out
		})
		sh(t, n, lines[1])
		if left := strings.TrimSpace(sh(t, n, "docker ps -aq; docker volume ls -q")); left != "" {
			t.Errorf("%s: the cleanup left %q behind", n.Hostname, left)
		}
	}
	if left := strings.TrimSpace(sh(t, mgr, "docker service ls -q; docker secret ls -q")); left != "" {
		t.Errorf("the cleanup left a service or secret behind: %q", left)
	}
}

// TestSwarmStackFileInstall installs the agent from the generated stack file instead
// of the command: the secret with the one-line command in its first comment, the
// file deployed as a stack. The service is <stack>_bayplan-agent, which the agent
// lookup must still find by its label (02-09, 02-14, the label fix).
func TestSwarmStackFileInstall(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr := f.sw.Managers[0]
	stack := f.env.Install.Swarm.StackFile

	first, _, _ := strings.Cut(stack, "\n")
	secretCmd, ok := strings.CutPrefix(first, "# Create the secret first:  ")
	if !ok {
		t.Fatalf("the stack file does not start with the secret command: %q", first)
	}
	sh(t, mgr, " "+secretCmd)
	if err := mgr.WriteFile(execCtx(t, time.Minute), "/tmp/bayplan-agent.yml", []byte(stack)); err != nil {
		t.Fatal(err)
	}
	t.Logf("stack deploy: %s", quiet(sh(t, mgr, "docker stack deploy -c /tmp/bayplan-agent.yml bayplan 2>&1")))

	got := f.waitSwarmOnline(t, 4)
	if got.EnvType != "swarm" {
		t.Errorf("envType = %q, want swarm", got.EnvType)
	}
	if names := strings.TrimSpace(sh(t, mgr, "docker service ls --format '{{.Name}}'")); names != "bayplan_bayplan-agent" {
		t.Errorf("services = %q, want bayplan_bayplan-agent", names)
	}
	// The hardening carried over to the stack file, with no flag the service form has and it lacks.
	spec := inspectService(t, mgr, "bayplan_bayplan-agent")
	ct := spec.TaskTemplate.ContainerSpec
	if spec.Mode.Global == nil || !ct.ReadOnly || !slices.Equal(sorted(ct.CapabilityDrop), []string{"ALL"}) || !slices.Equal(sorted(stripCap(ct.CapabilityAdd)), []string{"SETGID", "SETUID"}) {
		t.Errorf("stack service = mode %+v readonly %v caps drop %v add %v, want global, read-only, drop ALL add SETUID SETGID", spec.Mode, ct.ReadOnly, ct.CapabilityDrop, ct.CapabilityAdd)
	}
	if !slices.Contains(spec.TaskTemplate.Placement.Constraints, "node.platform.os == linux") && !slices.Contains(spec.TaskTemplate.Placement.Constraints, "node.platform.os==linux") {
		t.Errorf("constraints = %v", spec.TaskTemplate.Placement.Constraints)
	}
	if spec.UpdateConfig.Parallelism != 1 || spec.UpdateConfig.Delay != 10*time.Second {
		t.Errorf("update config = %+v", spec.UpdateConfig)
	}
	// The label lookup finds the agent tasks of a service the name does not give away.
	read, err := dockerxFor(t, mgr).Cluster(execCtx(t, time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(read.AgentTasks) != 4 {
		t.Fatalf("agent tasks found for a stack-file install = %d, want 4: %+v", len(read.AgentTasks), read.AgentTasks)
	}
	for _, task := range read.AgentTasks {
		if task.State != "running" {
			t.Errorf("agent task %+v, want running", task)
		}
	}
	// The state volume keeps the name the install gives it, not the stack's prefix.
	if vols := strings.Fields(sh(t, mgr, "docker volume ls -q")); !slices.Equal(vols, []string{"bayplan-agent"}) {
		t.Errorf("volumes on the manager = %v, want only bayplan-agent", vols)
	}
	if ports := strings.TrimSpace(sh(t, f.sw.Workers[0], "docker ps --filter label=bayplan.agent=true --format '{{.Ports}}'")); ports != "" {
		t.Errorf("the stack-file agent publishes %q", ports)
	}
}

// TestSwarmNodeStopped stops the worker's whole engine: the environment reports the
// node as not reporting, the dashboard tiles still add up, and when the node comes
// back, at its old address, its agent returns without a new enrolment.
func TestSwarmNodeStopped(t *testing.T) {
	f := startSwarmFix(t, 3, 1)
	mgr, worker := f.sw.Managers[0], f.sw.Workers[0]
	f.install(t)
	f.waitSwarmOnline(t, 4)
	ctx := execCtx(t, 10*time.Minute)

	if err := dockertest.StopNode(ctx, worker); err != nil {
		t.Fatal(err)
	}
	var env servertest.Environment
	f.srv.WaitFor("the environment to report the stopped worker", installWait, func() (bool, string) {
		e, ok := f.srv.Environment(f.env.ID)
		env = e
		return ok && e.Agents != nil && e.Agents.Connected == 3 && e.HasIssue("agent_offline"), summarise(e)
	})
	t.Logf("with the worker stopped: %s", summarise(env))
	if env.Status.State != "degraded" {
		t.Errorf("state with a stopped worker = %q, want degraded", env.Status.State)
	}

	// Lists name the node that is silent; the tiles add up to the per-node lines.
	f.srv.WaitFor("the lists to name the silent worker", 30*time.Second, func() (bool, string) {
		list, code, body := f.srv.Containers(f.env.ID, "")
		return code == http.StatusOK && slices.Contains(list.NotReporting, worker.Hostname), string(body)
	})
	sum, code, body := f.srv.Summary(f.env.ID)
	if code != http.StatusOK {
		t.Fatalf("summary = %d %s", code, body)
	}
	if !slices.Contains(sum.NotReporting, worker.Hostname) {
		t.Errorf("summary notReporting = %v, want %s", sum.NotReporting, worker.Hostname)
	}
	var total int
	for _, n := range sum.ByNode {
		total += n.Containers
	}
	if total != sum.Counts.Containers || sum.Counts.Containers != 3 || sum.Counts.ContainersRunning != 3 {
		t.Errorf("tooltip lines add up to %d, the tiles say %d (%d running), want 3 from the three managers: %s", total, sum.Counts.Containers, sum.Counts.ContainersRunning, body)
	}
	if sum.Counts.Nodes == nil || *sum.Counts.Nodes != 4 {
		t.Errorf("summary nodes = %v, want 4: a manager still counts the stopped one", sum.Counts.Nodes)
	}

	// The node comes back at the address its swarm knows, and so does its agent.
	if err := dockertest.StartNode(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if err := f.sw.WaitForQuorum(ctx); err != nil {
		t.Fatal(err)
	}
	ids := func() []string {
		var out []string
		for _, a := range f.srv.Agents(f.env.ID) {
			out = append(out, a.ID)
		}
		slices.Sort(out)
		return out
	}
	f.waitSwarmOnline(t, 4)
	if got := ids(); len(got) != 4 {
		t.Errorf("agent rows after the restart = %v, want the same four (no re-enrolment)", got)
	}
	_ = mgr
}
