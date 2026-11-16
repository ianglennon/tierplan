# Running TierPlan inside a swarm

TierPlan can run as a service in a Docker Swarm, on a manager node or on a worker node. This page gives a stack file you can deploy as it is, and the steps that come after: adding the swarm to TierPlan, then telling TierPlan that the node it runs on is part of that swarm.

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
