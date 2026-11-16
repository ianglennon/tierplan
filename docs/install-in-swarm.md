# Running TierPlan inside a swarm

TierPlan can run as a service in a Docker Swarm, on a manager node or on a worker node. This page gives a stack file you can deploy as it is, and the steps that come after: adding the swarm to TierPlan, then telling TierPlan that the node it runs on is part of that swarm.

- [Before you start](#before-you-start)
- [The stack file](#the-stack-file)
- [Deploy](#deploy)
- [Add the swarm](#add-the-swarm)
- [Fold this host into the swarm](#fold-this-host-into-the-swarm)
- [Fold a host that has ingress set up](#fold-a-host-that-has-ingress-set-up)
- [What each node role gives](#what-each-node-role-gives)
- [What not to do](#what-not-to-do)

## Before you start

- **Run one replica, on one node.** TierPlan keeps its data in SQLite, which has one writer, and its data sits in a volume on one node.
- **The socket mount is root-equivalent.** TierPlan, and every admin who signs in to it, gets the control of that node that root has. See [docs/threat-model.md](threat-model.md).
- **TierPlan does not deploy itself.** You deploy the stack by hand, on a manager, with `docker stack deploy -c tierplan.yml tierplan`. TierPlan never deploys, drains or updates its own node or its own service.

## The stack file

Save this as `tierplan.yml`, and replace `NODE-HOSTNAME` with the host name of the node TierPlan should live on (`docker node ls` shows it):

```yaml
services:
  tierplan:
    image: ghcr.io/ianglennon/tierplan:0.1
    ports:
      - target: 7480
        published: 7480
        mode: host
    volumes:
      - tierplan-data:/data
      - /var/run/docker.sock:/var/run/docker.sock
    stop_grace_period: 30s
    deploy:
      replicas: 1
      placement:
        constraints:
          - node.hostname == NODE-HOSTNAME
      update_config:
        order: stop-first
        parallelism: 1
      restart_policy:
        condition: any

volumes:
  tierplan-data:
```

What each part is for:

| Line | Why it is there |
|------|-----------------|
| `image: ghcr.io/ianglennon/tierplan:0.1` | An `X.Y` tag, which follows the patch releases of one minor line. There is no `:latest` tag while TierPlan is 0.x. |
| `replicas: 1` | TierPlan keeps its data in SQLite, which has one writer. Two replicas would fight over one database. Never raise this number. |
| `update_config.order: stop-first` | When the service is updated, the old task stops before the new one starts. With `start-first` two TierPlans would run on the same data for a moment. |
| `tierplan-data:/data` | A named volume holds the database and the encryption key. A named volume lives on one node, so it also ties TierPlan to that node. |
| `node.hostname == NODE-HOSTNAME` | Pins the service to the node that holds the volume. To use a node label instead, run `docker node update --label-add tierplan=true NODE-HOSTNAME` and write `node.labels.tierplan == true`. |
| `/var/run/docker.sock` | The socket of the node. It gives TierPlan the same control of that node as root has, so read [docs/threat-model.md](threat-model.md) first. |
| `mode: host` on the port | The port is opened on the node itself instead of on the swarm's routing mesh. TierPlan then sees the real address of each client, which its sign-in limit needs, and agent connections do not pass through the mesh. |
| `stop_grace_period: 30s` | Gives TierPlan time to close its database cleanly when the task stops. |
| `restart_policy.condition: any` | Swarm starts the task again whenever it stops. |

## Deploy

1. Save the stack file above as `tierplan.yml` and set `NODE-HOSTNAME` to the node TierPlan should run on. It can be a manager or a worker.
2. On a manager, run:

   ```sh
   docker stack deploy -c tierplan.yml tierplan
   docker service ps tierplan_tierplan
   ```

3. Open `https://NODE-HOSTNAME:7480` and claim the new instance, as in [First run](../README.md#first-run). The setup token is in the service log: `docker service logs tierplan_tierplan`.

The stack is called `tierplan` here, so the service is `tierplan_tierplan`. To update TierPlan later, change the tag in the file and run the same `docker stack deploy` command again. The old task stops first, then the new one starts, on the same volume. See [docs/upgrade.md](upgrade.md).

## Add the swarm

A swarm node is not managed as a separate "Local" host. On a swarm node, setup and **Settings** show **Add this swarm** where a standalone host shows **Manage this host**.

1. Choose **Add this swarm**. It opens the install dialog for a swarm, with the agent's swarm command: a global service, one agent on every node.
2. Run that command on a **manager** of the swarm. A worker cannot create services.
3. The page watches for the agent and opens the environment when it connects. See [Install on a swarm](agent.md#install-on-a-swarm) for what each flag does.

TierPlan refuses to add the socket of a swarm node as an environment of its own, through the API as well as in the screen. That is what keeps every stack from being listed twice, once for the swarm and once for the node. The one exception is a node that is already folded into a swarm environment: adding it again is how you undo a fold by hand.

## Fold this host into the swarm

Once the swarm environment lists the node TierPlan runs on, its settings page shows the **Local host** card. Choose **Fold into** the swarm.

- TierPlan reaches this node through the socket from now on, instead of through an agent.
- The agent the global service runs on this node keeps running as a standby. TierPlan does not use it. It is not revoked and not removed, because the service would only start it again.
- Nothing changes on the host, and the fold is recorded in the audit log.

**An install that lists every stack twice** (a Local environment next to the swarm) is fixed the same way, by hand: open the swarm environment's settings and fold the local host from the Local host card. This works on a worker node as well as on a manager.

**Folding an existing Local environment removes it, and everything stored under it:** its stacks, Git repositories, registry logins, janitor settings and the other records that hang off it. The dialog shows the counts and asks you to confirm. Running containers are not touched: only TierPlan's records go. If the Local environment has ingress set up, read the next section first.

## Fold a host that has ingress set up

A fold is refused while the Local environment holds ingress domains, reserved names, host names or routes, or has ingress turned on. Folding deletes the Local environment, and these would go with it. (An ingress setting that is off and only carries a version counter does not block the fold.) The refusal names what exists.

Do it in this order:

1. List the Local environment's ingress domains and reserved names through the API, and write them down.
2. Delete them, and turn ingress off for the Local environment.
3. Fold the local host into the swarm.
4. Register the same domains and reserved names on the swarm environment, and turn ingress on there.

## What each node role gives

| TierPlan runs on | Node-local calls (containers, logs, exec, janitor on this node) | Cluster calls (services, nodes, secrets, configs, networks) |
|------------------|-----------------------------------------------------------------|-------------------------------------------------------------|
| A manager | Through the socket | Through the socket, and also through the manager agents |
| A worker | Through the socket | Through the manager agents. A worker's socket cannot answer them |

A promotion or a demotion of the node is picked up on the next probe. You do not fold again.

## What not to do

- **Never drain, update or remove TierPlan's own node, or its own service, from TierPlan.** Doing so stops the thing that is asking. Change the node and the service with `docker node` and `docker stack deploy`, by hand.
- **Never run more than one replica.** SQLite has one writer.
- **Never point agents at an ingress hostname.** An agent connection that goes through the routing mesh or a router in front of TierPlan can be dropped or re-routed. Agents dial the address in `TIERPLAN_AGENT_URL`, which should name the node (or a name that goes straight to it), on the published port.
- If the node leaves the swarm, TierPlan shows it as degraded with the reason. It does not unfold it for you.
