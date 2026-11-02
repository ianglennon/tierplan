<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# The janitor

The janitor clears away what Docker leaves behind: exited task containers that pile up when swarm tasks recycle, dangling images, and networks no stack owns any more. It never removes a volume, a running container, anything younger than its grace period, or anything that belongs to a deploy in progress. This page is where the janitor is documented; for now it holds the one rule every other part depends on, how you keep something away from it.

The exact list of what each role may do is in [permissions.md](permissions.md), which is generated from the code. [networks.md](networks.md) covers the orphaned and unused networks the Address space page lists.

- [Keeping things from the janitor](#keeping-things-from-the-janitor)
- [What TierPlan does with the label itself](#what-tierplan-does-with-the-label-itself)

## Keeping things from the janitor

Put the label `tierplan.keep=true` on anything you want the janitor to leave alone. It is the one opt-out label for every kind of object the janitor judges: containers, images and networks. There is no second label for another kind of object.

```yaml
services:
  web:
    image: nginx:1.27
    labels:
      tierplan.keep: "true"
networks:
  back:
    labels:
      - tierplan.keep=true
```

The rules are exact, because the label is a promise that something will not be deleted:

- **The value must be the exact text `true`.** `True`, `TRUE`, `1`, `yes`, an empty value, or `true` with a space around it protects nothing. The key is compared the same way: it is `tierplan.keep`, in lower case, nothing else.
- **Both ways of writing it in a compose file work.** `tierplan.keep: true` (a YAML boolean) and `tierplan.keep: "true"` (a string) both arrive on the object as the text `true`, as does the list form `- tierplan.keep=true`. You do not need to quote it.
- **The label protects only the object it is on.** A label on a service protects that service's containers, not the other services of the stack. A label on a network protects that network, not the containers attached to it. Putting it on one object never keeps its neighbours.
- **A label that is on the object when the janitor looks is what counts.** If you remove it, the object is judged like any other from then on.

### Keeping a whole stack

To leave everything a stack owns alone, do not label each object. Exclude the stack:

- the stack's **janitor switch** in TierPlan, or
- `x-tierplan: {keep: true}` at the top level of the stack's compose file, which lets a stack kept in git declare it in the repository.

Both are added to this release alongside the janitor's pages; until they are, label the objects you want kept. The label on its own never excludes a whole stack, and neither do the services' labels together; only these two do.

## What TierPlan does with the label itself

TierPlan writes some labels of its own on everything a stack makes (`tierplan.stack.id` and the rest, listed in [stacks.md](stacks.md)). Your `tierplan.keep` is not one of them, and TierPlan treats it as yours:

- **Adopting a stack** that is already running keeps the label in the compose file TierPlan writes for it, on services, containers, networks and volumes.
- **Rebuilding a stack's file** from what is running keeps it too.
- **Drift** compares the label like any setting you wrote: if the stack's definition has `tierplan.keep=true` and a running service has lost it, the service shows as changed, so a label cannot disappear without it being shown.
- **Deploying** puts the label where you wrote it, as the text `true`: `labels:` on a service reaches its containers (the exited task containers the janitor reads), `deploy.labels:` reaches the swarm service, and `labels:` on a network reaches the network.

A label can keep an object off the janitor's list for as long as it stays on it. That is the point of it, and it is the same risk as any label you put on an object by hand: the object stays until you remove the label or remove the object yourself.
