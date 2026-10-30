# Updating and removing agents by hand

Coaming updates the agents of an environment itself when that is switched on for the
environment and the host owner allows it. This page is for the other cases: the header
bell says "{n} agents need a manual update", an update failed, or an update was
interrupted.

Switching automatic updates, updating one agent and updating every agent that is behind
need **Admin** on the environment (`environments.configure`), as do instance admins. The
requests go to `PATCH /api/env/{envId}`, `POST /api/env/{envId}/agents/{agentId}/update` and
`POST /api/env/{envId}/update-agents`.

## When an update is manual

The bell lists an agent here when it is older than the server and Coaming cannot update it:

- Auto-update is off for the environment, or the host owner set
  `COAMING_AGENT_SELF_UPDATE=false` on the agent.
- The agent is outside the compatibility window of this server, or too old to understand
  the update message.
- The agent's connection is not verified. Update and uninstall are accepted only on a
  session that checked Coaming's certificate (`wss://`), and an agent on a plain `ws://`
  connection answers `insecure_session`.
- The last update did not finish (the node list says why: the new agent never connected,
  or it rolled back to the old version).

The node list under Settings, Environments shows the one-line command for each such agent.

## Updating by hand

On a swarm, from a manager:

```sh
docker service update --image ghcr.io/ianglennon/coaming-agent:<version> coaming-agent
```

On a standalone host, the node list gives the full command. It pulls the image, removes the
old container and starts a new one with the same flags. The agent's state volume holds its
identity, so no join token is needed.

A standalone self-update pulls the new image anonymously, so an agent image in a private
registry has to be pulled by hand first.

## A swarm update that paused

A swarm rolls with Docker's own rolling update, and Docker's default when a task fails to start is to pause the update. With a broken agent image the first node's agent stops, the update pauses, and the other nodes keep the old agent. The node list shows the first node as disconnected, and after fifteen minutes the agent that was asked to start the update shows "the agent never came back on the new version". On a manager:

```sh
docker service ps coaming-agent --no-trunc      # the failed task says why
docker service rollback coaming-agent           # puts the service back on the previous image
```

(`<stack>_coaming-agent` for an install from the stack file.) The agent on the failed node returns within a minute.

## Recovering an interrupted standalone update

A standalone update renames the running agent to `coaming-agent-old`, stops it, creates the
new one under the old name, and removes the old one when the new one has connected. If the
host reboots or the helper container is killed in the middle, the old agent can be left
renamed and stopped. On that host:

```sh
docker rm -f coaming-agent        # only if a new one exists but never connected
docker rename coaming-agent-old coaming-agent
docker start coaming-agent
```

A leftover `coaming-agent-old` also stops the next update: its rename would fail. If an
update succeeded but the old container could not be removed, the agent log says so. Remove
it with:

```sh
docker rm -f coaming-agent-old
```

## Removing an agent by hand

Removing an environment can uninstall its agents when they are online. For a host that was
offline, or when you chose not to, run the commands from the environment's "Manual cleanup
command" section on each host.

On a swarm (the secret is named for the join token's current generation, which the section
shows):

```sh
docker service rm coaming-agent && docker secret rm <current secret>
docker volume rm coaming-agent   # on every node that ran the agent
```

On a standalone host:

```sh
docker rm -f coaming-agent && docker volume rm coaming-agent
```

An agent installed from the stack file runs as `<stack>_coaming-agent`. Remove it with
`docker stack rm <stack>` and remove the stack's network and the `coaming-agent` volume if
they are left behind.
