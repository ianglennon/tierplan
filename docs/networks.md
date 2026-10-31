<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Coaming Authors -->

# Networks and address pools

Every Docker network takes a block of addresses from a pool. When a pool runs out, nothing new can be created, and
on a swarm a service that cannot get an address on its overlay cannot start a task. This page explains how
Coaming shows where an environment's address space has gone, when it warns you, which networks it calls orphaned or
unused, how the subnet picker chooses a subnet, how the small stack subnets setting works, and what the pool
guidance covers. The plain list of networks, with create and delete, is in [containers.md](containers.md); this page
is about the space.

The exact list of what each role may do is in [permissions.md](permissions.md), which is generated from the code.
[threat-model.md](threat-model.md) says what this adds to the risks of running Coaming.

- [The Address space page](#the-address-space-page)
- [Pools](#pools)
- [Warnings](#warnings)
- [Networks filling up](#networks-filling-up)
- [The subnet map](#the-subnet-map)
- [Orphaned and unused networks](#orphaned-and-unused-networks)
- [The subnet picker](#the-subnet-picker)
- [Small stack subnets](#small-stack-subnets)
- [Pool guidance](#pool-guidance)
- [What is not metered](#what-is-not-metered)

## The Address space page

Every environment has an **Address space** page under **Resources** in the sidebar. You need the `networks.read`
permission on the environment (a Viewer has it). From top to bottom it shows:

1. **Pools**: one meter for each address pool, and one for the swarm's ingress network.
2. **Networks filling up**: the overlays that are running out of addresses inside their own subnet, worst first.
3. **The subnet map**: every network of the environment, with its subnet, owner and use.
4. **Orphaned and unused networks**, and the networks you chose to keep.

The page refreshes by itself when a network, container or service changes. Docker is the source of truth: Coaming
reads the numbers from the engines, keeps them for a few seconds, and does not store a copy.

## Pools

A **pool** is a range that Docker cuts subnets out of. Each meter shows the pool, the size of the subnet cut from
it (the mask), how many of those subnets are used out of how many fit, and the **largest free block**. A network you
made with a custom subnet can sit in the middle of a pool and split it, so the largest free block can be smaller
than the free space suggests.

- **A swarm's pool is fixed when the swarm is created.** It is the `--default-addr-pool` given to `docker swarm
  init`, or `10.0.0.0/8` cut into `/24` subnets when none was given. Coaming reads it from a manager and the
  figures are measured. A swarm cannot change its pool afterwards; see [Pool guidance](#pool-guidance).
- **The ingress network** has its own row. It can sit outside the default pool, and the row says so when it does;
  when it sits inside a pool it is counted there once.
- **A standalone host's pools are always labelled Estimated.** The Docker API does not tell Coaming a host's
  `default-address-pools`. Coaming uses, in this order, the pools you entered for the environment (**Settings >
  Environments > the environment > Address pools**), the pools the daemon reports when it reports any, and
  Docker's own documented defaults (`172.17.0.0/16`, `172.18.0.0/16`, `172.19.0.0/16`, `172.20.0.0/14`,
  `172.24.0.0/14` and `172.28.0.0/14` cut into `/16` subnets, then `192.168.0.0/16` cut into `/20`s). It counts the subnets of the host's networks against those pools. If the host's `daemon.json` sets
  `default-address-pools`, enter the same pools in the settings and the estimate becomes a good one. The row says
  where the pools came from.
- **Host routes count as used.** On a standalone host, a block that a route of the host already takes (a VPN, for
  instance) is counted as taken when Coaming has read the host's routes (see [the subnet picker](#the-subnet-picker)).

**On a swarm, the meters are for the swarm pool only.** Each node's own local networks (`docker_gwbridge` and the
bridges of standalone containers) take their subnets from that node's daemon pools, not from the swarm pool, and
Coaming 0.1 **does not meter** those node-local pools. Their networks still appear in the subnet map and count in
the subnet picker's overlap checks. See [What is not metered](#what-is-not-metered).

A node that is not reporting is named on the page. The numbers are then partial, and Coaming says so instead of
showing a smaller figure. If no manager answers, the swarm's pool rows read **Not measured**.

## Warnings

Thresholds are fixed in this version and have no setting:

| Used | Meter | What it means |
| --- | --- | --- |
| below 70% | plain | nothing to do |
| 70% and above | **Getting full** (amber) | plan some space |
| 90% and above | **Almost full** (red) | act now |

The state is also a word and an icon, never only a colour. The same warning appears in three more places:

- an **Address space** card on the environment's dashboard, showing the fullest pool or network and a link to the
  page;
- a pool column on the **Environments** overview, which sorts an environment with a getting-full or almost-full pool
  in with the ones that need attention;
- **the environment's health.** A measured swarm pool or overlay at 90% makes the environment **degraded**, with
  the reason *address space full* naming the pool or network. It stays degraded until every measured meter is
  back **below 85%**. The gap is deliberate, so an overlay that hovers around 90% does not flip the health state
  every minute. The background check runs about once a minute and also when a network, container or service
  changes.
- **An estimate never makes an environment degraded.** A standalone host's pool is a guess, so it warns at 70% and
  90% on the card and the column but leaves the health alone. Only measured swarm pools and overlays degrade it.

## Networks filling up

A pool running out is one way to fail. The other is a single overlay running out of addresses inside its own subnet.
Every task, every service virtual IP and every node's endpoint takes an address, and a start-first update runs the
old and the new tasks together. A `/24` overlay holds 253 of them, and a stack of 100 replicas with a start-first
rollout will run it dry.

Coaming counts the used addresses of each swarm overlay and applies the same 70% and 90% states to it. The
warning names the network. This needs **Docker Engine 29**: older engines do not report address counts, so on
Engine 24 to 28 an overlay reads **Not measured** and has no number. The pool meters still work on those engines.
The **Networks** list shows the same figure in its **Addresses used** column.

## The subnet map

The map lists every network of the environment in numeric address order. A swarm overlay appears once. A
local-scope network (bridge, macvlan, ipvlan) appears once per node, with the node named, as in the Networks list.
Each row shows the subnet, scope, driver, **owner** (the stack, compose project or service that made it, or none),
how many things are attached, its age, and for an overlay a used-of-capacity bar.

- A **flag on a row** means its subnet overlaps another network's. The row names the other network. Overlap between
  two local networks on different nodes is normal and is not flagged, because they are different address spaces.
- **Filters**: type to search by name, subnet or owner, show **All**, **Overlays**, **Local**, **Filling up** or
  **Overlapping** networks, and on a swarm choose one node. Rows that are orphaned, unused or kept carry a badge. Column headers sort. The table draws 500 rows at a time; narrow it
  with a filter to see the rest. The map reads at most 5,000 networks.
- The owner link goes to the Coaming stack only when the network's label names a stack that Coaming holds. A label
  alone is not trusted to make a link.

## Orphaned and unused networks

The first goal of Coaming is that removing a stack leaves nothing behind. This section finds what was left behind
by something else: a stack removed outside Coaming, a network someone created and forgot.

**The two classes**

- **Orphaned**: the network carries a stack label (`com.docker.stack.namespace` or `com.docker.compose.project`),
  the stack with that name no longer exists, and nothing uses the network.
- **Unused**: no stack ever labelled it, and nothing uses it. This is information, not a fault.

**What counts as "nothing uses it".** Coaming looks at all of these, on **every node**:

- containers attached to the network, on each node;
- the current spec of every service, **and its previous spec** (a rolling update that has not finished, or can be
  rolled back, still holds the old network), and the service's virtual IPs;
- every task of a service that has not finished with the network, including a task that is shutting down.

A network is never listed when any of these uses it.

**What is never listed.**

- Docker's own networks: `bridge`, `host`, `none`, `ingress` and `docker_gwbridge`, matched by name and by what
  `docker network inspect` says about them (the ingress flag, config-only networks and the networks a
  `config-from` points to).
- A network that a stack Coaming holds declares `external: true`.
- A network owned by a stack that Coaming is deploying or changing right now.
- A network younger than the **grace period**.
- A network you chose to keep (below).

**The 24 hours.** A network is listed only after it has been idle for 24 hours. The clock starts at the later of the
network's creation and **the last time Coaming saw it in use**. Coaming records that in its own database, because
Docker does not. A network that was busy until an hour ago is therefore not listed today, and a long-lived
network is not flagged the moment it is freed.

**Can't tell yet.** Coaming does not judge a network when its picture is incomplete: a node that could hold an
attachment is not reporting, no manager answered, or a stack of the environment did not load, so what it declares
external is unknown. The row says **Can't tell yet** with the reason and cannot be selected or deleted. Bring the node back
or fix the stack and the next check decides. The page says how many networks are withheld.

**Keeping a network.** Two ways, both honoured by every check:

- give the network the label **`coaming.keep=true`**. It is never listed. Remove the label to list it again.
- press **Keep** on an orphaned or unused row (permission `networks.keep`, Env Admin). A label cannot be added to a
  network that exists, so Coaming records the exemption by name in its database. **Stop keeping** removes it. The
  **Kept** list at the bottom of the card shows both kinds and says which it is. A network that has both is shown
  once as kept by label.

A kept-by-name exemption is by **name** (with the node for a local network). If you delete the network and a new
one is created with the same name, it is kept too.

**Deleting.** **Delete…** on a row, or a selection of rows, needs `networks.manage` (Env Admin). Each network is
handled on its own:

1. Coaming reads everything again, fresh, and judges the one network again. If it is in use now, no longer
   orphaned, or a node is missing, nothing is removed and the report says **Kept** with the reason.
2. It saves the network's **full `docker network inspect` to the audit log** as `network.inspect_saved`, with
   secret-like values in its labels and options masked (a label or option block of an unexpected shape is withheld whole). If the audit log
   cannot take it, the network is not deleted.
3. It sends one delete. If Docker refuses because the network is in use, that counts as a safe result (**Kept**), and
   Coaming does not try again or force it.

**A network whose details are over 64 KB** (a network with a very large number of attached endpoints) is not
deleted from this page: the whole inspect could not be saved. Remove it with the Docker CLI after reading what it
holds. Built-in networks can never be deleted.

A bulk delete shows its progress and finishes with a report of what was deleted, kept and failed.
Scheduled or automatic removal is not in this version.

## The subnet picker

The picker is part of the **Create network** dialog (**Advanced**), of the inline **Create new** network in the
service editor (a closed **Subnet** section reading **Automatic** or the subnet you chose), and the compose editor
warns about a subnet written in the file.

- **Size.** Choose `/24`, `/25`, `/26`, `/27` or `/28`, each with its usable address count (`2^(32 - mask) - 3`: the
  network, broadcast and gateway addresses are not usable). On a swarm overlay the default is the pool's own mask.
- **Expected tasks.** Type how many tasks the network will carry and Coaming recommends the smallest size that
  fits. The number it needs is
  `ceil((2 x tasks + services + nodes) x 1.25) + 3`: twice the tasks for a start-first update, one virtual IP for each
  service (1 unless you say otherwise), one endpoint for each node (the swarm's node count unless you say
  otherwise), a quarter more to scale into, and the 3 reserved addresses. In the service editor the task count is
  filled in from the replicas.
- **Suggest.** Fills the field with the **lowest free block** of the chosen size inside the environment's pool,
  clear of every IPv4 subnet on every node and of the host routes that were read. Packing from the lowest address
  limits fragmentation. The suggestion reserves nothing, and you can edit the field. If no block of that size is
  free, the picker says so (never an error) and links to the Address space page and the pool guidance. A local
  driver (bridge, macvlan, ipvlan) on a swarm needs a node chosen first, because it takes its subnet from that node's pools.
- **Typed subnets** are checked as you type: a subnet with host bits set (`10.0.0.5/24`) is corrected to its network
  address, and one that is too small is refused. IPv6 subnets are not checked for overlaps, and the picker says so.

**The four overlap sources**

| Check | Kind | Effect |
| --- | --- | --- |
| This environment's networks, on every node | same environment | **Blocks** until you tick **Create it anyway**. Macvlan sometimes needs an overlap on purpose. The override is recorded in the audit log. |
| Other environments' networks | other environment | A note, for sites linked by a route or a VPN. Only environments you may read are named. |
| The host's own IPv4 routes, read by the agent | host route | A warning, named by interface. |
| A fixed list of home and VPN ranges (`192.168.0.0/16`, `172.16.0.0/12`, `10.0.0.0/8`) | common range | A warning. |

A fifth note, **outside the pool**, says when a subnet you typed is not inside any of the environment's pools.
On a default swarm the pool is `10.0.0.0/8`, so every subnet inside it falls in a common range. The picker shows that one as a quiet note, not a
warning, because it is the pool the swarm already uses.

Only the same-environment check ever blocks, and the server checks it again when you create the network, so two people creating
networks at the same moment cannot take the same block. Creating from the service editor does the same check.

**Host routes and the agent mount.** To read a host's routes, the agent needs one read-only file mounted:
`/proc/1/net/route` at `/host/route`. New agent install commands carry it. An agent installed before this release
has no such mount, and the picker says it **can't read that host's routes** and gives the one command that adds it
(see [agent.md](agent.md#host-routes)). Coaming's own host has no agent; add `-v /proc/1/net/route:/host/route:ro`
to the `docker run` of Coaming itself. Without the mount that check is skipped with a note, and nothing else changes.

## Small stack subnets

By default a new swarm network takes a `/24` and a new standalone bridge takes a `/16`, far more than a small
stack uses. A standalone host runs out of bridge pools at about 30 networks. The **Default stack subnets** setting
(**Settings > Environments > the environment**; it needs the `networks.addressing` permission, Env Admin, and is
recorded in the audit log) gives each new stack network a small subnet instead.

- **Off by default.** Turn it on and choose `/25`, `/26`, `/27` or `/28`. The default is `/26`, 61 usable
  addresses. The card says how many more networks of that size the pool has room for.
- **New networks only.** When a deploy needs to create a network that has no `ipam` block of its own and is not
  `external`, it takes the lowest free block of that size (the same choice as **Suggest**). **A network that already
  exists is never recreated, and a network with an explicit `ipam` subnet keeps the one you wrote.** **Your YAML is
  never rewritten**: the subnet is added when the deploy is sent, not saved in the file, and a git stack stays
  read-only.
- **The review shows it.** A new network's row reads, for instance, `network backend: 10.0.5.64/26` with a
  **Small-subnet default** badge. If there is no free block of that size, the review stops with *no free block* before
  anything is created, and links to the Address space page.
- **It is recorded for rollback.** The subnet each network was given is saved with the revision. Roll back to that
  revision, or deploy again after the network is gone, and the same block is used again if it is still free and
  inside a pool, otherwise the next free one. The History tab lists them under **Networks**, and marks them
  **reused on rollback** on a rollback revision. Because History keeps only the network name and subnet, a block
  that a rollback had to re-pick is marked as reused too.
- **Standalone hosts.** The same setting applies to a standalone stack's networks. Coaming adds the recorded subnet
  to a network on every later deploy, whether the setting is on or off, so the Compose engine's own
  configuration hash for the network never changes and a later deploy does not recreate it. A real engine test checks that.
  Networks that already exist keep their subnets. A standalone network with an `ipam` block, or one that is not a
  plain default bridge network, is left alone.
- **Capacity.** A `/26` holds 61 addresses. A service with 30 replicas updating start-first needs about 60. If you
  scale a stack beyond its network, it fails to start tasks. Turn the setting off or choose a larger size for stacks
  that scale, or write an `ipam` subnet in the file for the one network.

**A known gap.** Starting a **stopped swarm stack** that must make a missing network again (**Start** on the stack
page, after the network was removed behind Coaming's back) creates it without a small subnet: it takes the swarm's
default size. A normal deploy is unaffected. Deploy the stack instead of starting it if you need the small block.

## Pool guidance

A swarm's address pool cannot be changed in place. The only way to change it is to rebuild the swarm. A
standalone host's pools live in `daemon.json`. **Coaming never runs `docker swarm init` or `docker swarm leave`, and
never edits `daemon.json`**: there is no *edit pool* button anywhere. The **Pool guidance** page (the button in the
Address space header, and the links from the warnings) explains and generates commands you copy and run yourself. It has four parts:

1. **Free space first.** What you can do without rebuilding: remove orphaned and unused networks (with the number of
   addresses they hold), turn on [small stack subnets](#small-stack-subnets), and the overlays that are already at 70%
   or more, each linked to the network.
2. **New swarm.** Tell it how many networks and nodes you expect, the subnet size and the ranges to avoid, and it
   generates a `docker swarm init --default-addr-pool … --default-addr-pool-mask-length …` command, picking the lowest block in
   `10.0.0.0/8` then `172.16.0.0/12` that holds 1.5 times your networks and overlaps nothing you avoid. It explains
   that the pool is fixed at init.
3. **Rebuild a swarm.** A runbook for an existing swarm: back up, remove stacks, leave and re-init on the new pool,
   rejoin the nodes, redeploy. It says plainly that **this means downtime**. It prints a `docker stack rm` for each
   Coaming stack whose name is plain; the rest it sends you to the Stacks page for.
4. **Standalone host.** A `daemon.json` `default-address-pools` snippet built from the pools you type (they are
   checked the way the settings card checks them), the note that the Docker daemon must be restarted, and a link to enter the same
   pools in the environment settings so the estimate is good.

The page only reads. Nothing you type is sent anywhere, and a value that is not a plain address or name makes an
error line instead of a command.

## What is not metered

- **Each swarm node's own local networks.** On a swarm, `docker_gwbridge` and the bridge networks of standalone
  containers take their subnets from **that node's own daemon pools**, not from the swarm pool. Coaming 0.1 **does not meter** those node-local pools. Their networks still appear in the subnet map and count in the subnet picker's
  overlap checks, but a node whose local pool is running dry gives **no warning**. Watch for it if you run many
  standalone containers on swarm nodes.
- **VXLAN IDs** and anything else that is not an address.
- **Standalone numbers are estimates** (see [Pools](#pools)).
- The warning thresholds are fixed at 70% and 90%.
- **Overlay counts need Docker Engine 29.** Older engines read *Not measured*.
