# Updating and removing agents by hand

Bayplan updates the agents of an environment itself when that is switched on for the
environment and the host owner allows it. This page is for the other cases: the header
bell says "{n} agents need a manual update", an update failed, or an update was
interrupted.

## When an update is manual

The bell lists an agent here when it is older than the server and Bayplan cannot update it:

- Auto-update is off for the environment, or the host owner set
  `BAYPLAN_AGENT_SELF_UPDATE=false` on the agent.
- The agent is outside the compatibility window of this server, or too old to understand
  the update message.
- The agent's connection is not verified. Update and uninstall are accepted only on a
  session that checked Bayplan's certificate (`wss://`), and an agent on a plain `ws://`
  connection answers `insecure_session`.
- The last update did not finish (the node list says why: the new agent never connected,
  or it rolled back to the old version).

The node list under Settings, Environments shows the one-line command for each such agent.

## Updating by hand

On a swarm, from a manager:

```sh
docker service update --image ghcr.io/ianglennon/bayplan-agent:<version> bayplan-agent
```

On a standalone host, the node list gives the full command. It pulls the image, removes the
old container and starts a new one with the same flags. The agent's state volume holds its
identity, so no join token is needed.

A standalone self-update pulls the new image anonymously, so an agent image in a private
registry has to be pulled by hand first.

## Recovering an interrupted standalone update

A standalone update renames the running agent to `bayplan-agent-old`, stops it, creates the
new one under the old name, and removes the old one when the new one has connected. If the
host reboots or the helper container is killed in the middle, the old agent can be left
renamed and stopped. On that host:

```sh
docker rm -f bayplan-agent        # only if a new one exists but never connected
docker rename bayplan-agent-old bayplan-agent
docker start bayplan-agent
```

A leftover `bayplan-agent-old` also stops the next update: its rename would fail. If an
update succeeded but the old container could not be removed, the agent log says so. Remove
it with:

```sh
docker rm -f bayplan-agent-old
```

## Removing an agent by hand

Removing an environment can uninstall its agents when they are online. For a host that was
offline, or when you chose not to, run the commands from the environment's "Manual cleanup
command" section on each host.

On a swarm (the secret is named for the join token's current generation, which the section
shows):

```sh
docker service rm bayplan-agent && docker secret rm <current secret>
docker volume rm bayplan-agent   # on every node that ran the agent
```

On a standalone host:

```sh
docker rm -f bayplan-agent && docker volume rm bayplan-agent
```

An agent installed from the stack file runs as `<stack>_bayplan-agent`. Remove it with
`docker stack rm <stack>` and remove the stack's network and the `bayplan-agent` volume if
they are left behind.
