# Updating and removing agents by hand

TierPlan updates the agents of an environment itself when that is switched on for the
environment and the host owner allows it. This page is for the other cases: the header
bell says "{n} agents need a manual update", an update failed, or an update was
interrupted.

Switching automatic updates, updating one agent and updating every agent that is behind
need **Admin** on the environment (`environments.configure`), as do instance admins. The
requests go to `PATCH /api/env/{envId}`, `POST /api/env/{envId}/agents/{agentId}/update` and
`POST /api/env/{envId}/update-agents`.

## When an update is manual

The bell lists an agent here when it is older than the server and TierPlan cannot update it:

- Auto-update is off for the environment, or the host owner set
  `TIERPLAN_AGENT_SELF_UPDATE=false` on the agent.
- The agent is outside the compatibility window of this server, or too old to understand
  the update message.
- The agent's connection is not verified. Update and uninstall are accepted only on a
  session that checked TierPlan's certificate, and an agent on any other session answers
  `insecure_session`. Agents connect over `wss://` only, so this is rare: an agent that
  cannot verify the certificate does not connect at all.
- The last update did not finish (the node list says why: the new agent never connected,
  or it rolled back to the old version).

The node list under Settings, Environments shows the one-line command for each such agent.

## Updating by hand

On a swarm, from a manager:

```sh
docker service update --image ghcr.io/ianglennon/tierplan-agent:<version> tierplan-agent
```

On a standalone host, the node list gives the full command. It pulls the image, removes the
old container and starts a new one with the same flags. The agent's state volume holds its
identity, so no join token is needed.

A standalone self-update pulls the new image anonymously, so an agent image in a private
registry has to be pulled by hand first.

## A swarm update that paused

A swarm rolls with Docker's own rolling update, and Docker's default when a task fails to start is to pause the update. With a broken agent image the first node's agent stops, the update pauses, and the other nodes keep the old agent. The node list shows the first node as disconnected, and after fifteen minutes the agent that was asked to start the update shows "the agent never came back on the new version". On a manager:

```sh
docker service ps tierplan-agent --no-trunc      # the failed task says why
docker service rollback tierplan-agent           # puts the service back on the previous image
```

(`<stack>_tierplan-agent` for an install from the stack file.) The agent on the failed node returns within a minute.

## Recovering an interrupted standalone update

A standalone update renames the running agent to `tierplan-agent-old`, stops it, creates the
new one under the old name, and removes the old one when the new one has connected. If the
host reboots or the helper container is killed in the middle, the old agent can be left
renamed and stopped. On that host:

```sh
docker rm -f tierplan-agent        # only if a new one exists but never connected
docker rename tierplan-agent-old tierplan-agent
docker start tierplan-agent
```

A leftover `tierplan-agent-old` also stops the next update: its rename would fail. If an
update succeeded but the old container could not be removed, the agent log says so. Remove
it with:

```sh
docker rm -f tierplan-agent-old
```

## Removing an agent by hand

Removing an environment can uninstall its agents when they are online. For a host that was
offline, or when you chose not to, run the commands from the environment's "Manual cleanup
command" section on each host.

On a swarm (the command finds the agent service by its `tierplan.agent=true` label, so it also
removes a stack-deployed `<stack>_tierplan-agent`, and reads the secret names from the service, so
it works for any generation of the join token and for installs made with either naming of the
secret; it ends with a non-zero status when a step failed):

```sh
if L=$(docker service ls -q --filter label=tierplan.agent=true); then rc=0; [ -n "$L" ] || echo 'no agent service (label tierplan.agent=true) on this swarm' >&2; else rc=1; fi
for v in $L; do
  S=$(docker service inspect -f '{{range .Spec.TaskTemplate.ContainerSpec.Secrets}}{{.SecretName}} {{end}}' "$v") && docker service rm "$v" || { rc=1; continue; }
  for s in $S; do docker secret rm "$s"; done
done; [ "$rc" = 0 ]
docker volume rm tierplan-agent   # on every node that ran the agent
```

On a standalone host:

```sh
docker rm -f tierplan-agent && docker volume rm tierplan-agent
```

An agent installed from the stack file runs as `<stack>_tierplan-agent`. The command above
finds it by its label; `docker stack rm <stack>` also removes it. Remove the stack's network
and the `tierplan-agent` volume if they are left behind.
