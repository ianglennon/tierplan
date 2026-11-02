<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# Swarm services, nodes, secrets and configs

This page explains what TierPlan does with the services of a Docker Swarm, the nodes that run
them, and the secrets and configs they use: who may do each thing, what asks for
confirmation first, and what is written down about it. It also states plainly three things that
are decisions and not oversights: [a host's owner decides whether TierPlan may change a service
that has host access](#changing-services-with-host-access), [config content is readable by
everyone who can see the config](#secrets-and-configs), and [service logs are shown as the
application wrote them](#service-logs).

These pages exist only on a swarm environment. On a standalone host the sidebar has no Services, Nodes,
Placement, Secrets or Configs entries, and opening one of their addresses says the environment is not
a swarm.

The exact list of what each role may do is in [permissions.md](permissions.md), which is generated
from the code. [access.md](access.md) explains roles and extra permissions. [agent.md](agent.md)
explains what the agent on a manager or worker will and will not forward. [containers.md](containers.md)
covers the containers a service's tasks run in.

Every change on this page goes through a typed route, not through a raw Docker proxy, and is
checked against the person's role on that environment first. A change to a swarm is sent through one
manager's agent, once. Nothing is retried: if the connection to the manager is lost while TierPlan is
changing something, the answer says the manager did not answer, and you look at the swarm before you
try again. If no manager's agent is connected, or the swarm has lost its quorum, the pages show what
TierPlan last saw and every change is off, with that as the reason.

- [Services](#services)
- [Changing services with host access](#changing-services-with-host-access)
- [The editor](#the-editor)
- [Tasks and placement](#tasks-and-placement)
- [Nodes](#nodes)
- [Secrets and configs](#secrets-and-configs)
- [Service logs](#service-logs)
- [Stats](#stats)
- [Restart loops](#restart-loops)
- [What is recorded](#what-is-recorded)

## Services

### The list

- **Grouping.** Services sit under their stack, taken from the `com.docker.stack.namespace` label.
  A group header says how many of its services are healthy. **Group by** also offers **None**. The
  choice, the search and the filters are in the address, so you can share a link.
- **Search and filters.** The search box matches the name, image, label keys and values, and the
  stack. The server does this before it cuts the list. The status chips are **Degraded**,
  **Updating**, **Update problem** and **Restart loop**, and their numbers do not shrink as you type
  in the search box.
- **State.** A service reads Running, Degraded (some but not all of its replicas run), Not running,
  Scaled to 0, Updating, Rolling back, Update paused, Rollback paused or Rolled back. A job reads
  Running or Completed. Hovering the state says how many replicas run.
- **Large swarms.** The list shows at most 500 rows and says so under the table.
- **Live updates.** Rows change in place when Docker reports a change. While a menu or dialog is
  open or rows are selected, the list holds still, and a **changes waiting** pill applies them.
- **Live stats.** The **Live stats** switch adds CPU, Memory and Network columns for the rows you
  can see (see [Stats](#stats)).

### The service page

The page has the tabs **Overview** (state, image with its digest, mode, ports, resources, update and
restart policy, placement rules, networks, mounts, and links to the secrets and configs it uses),
**Tasks**, **Placement**, **Logs**, **Stats** and **Inspect** (the engine's JSON). Anyone who can see
the environment (`services.read`) can open all of them.

The values of environment variables, and of the command, arguments, health check test and log-driver
options, are hidden for everyone who does not hold the **Reveal secrets** extra permission, on the
Overview, in the Inspect JSON, in the editor and in every difference TierPlan shows. They appear as
`••••` in the form and as `<hidden>` in the Spec tab. Labels are shown as they are, so do not put a
secret in a label.

### Actions and who may run them

| Action | Needs | Asks first |
|--------|-------|------------|
| Scale | Operator (`services.control`) | A dialog with the replica count, 0 to 1000 |
| Force update, Retry update | Operator (`services.control`) | Yes, a dialog |
| Roll back | Operator (`services.control`) | Yes, a dialog that shows what changes |
| Edit | Operator (`services.control`) | The review step, see [The editor](#the-editor) |
| Mute restart-loop alerts | Operator (`services.control`) | No |
| Create service | Admin (`services.manage`) | The review step |
| Remove | Admin (`services.manage`) | Yes, a dialog |

A Viewer sees the buttons disabled, with the reason ("Needs Operator" or "Needs Admin").

- **Scale** sets the replica count. It is hidden for global services and jobs. Scaling to 0 stops
  every task but keeps the service, and the dialog says so. Several selected services can be
  scaled together from the bulk bar: each row shows its new count, a service that cannot be scaled is
  listed as skipped, one that fails never stops the others, and a toast sums up ("Scaled 4, 1 failed").
  The bulk shares one **bulk ID** in the audit log, and a selection of more than 100 is sent in
  several requests that share it.
- **Force update** makes Swarm replace every task, a batch at a time, using the service's own
  update settings. TierPlan looks the image up again and pins the result: if the tag has moved since the
  last update, the new image is what runs. An image that is already given as a digest is sent as it
  is. Repeating a force update starts another rollout each time. When an update has paused, the same
  dialog is offered as **Retry update**.
- **Roll back** puts the service back to the settings it had before its last update, and shows the
  current settings against the earlier ones before you confirm. It is disabled, with the reason,
  when Swarm holds no earlier version. If the earlier version names a secret, config or network that
  has since been removed, Swarm refuses and TierPlan says which object is missing.
- **Remove** deletes the service; Swarm stops its tasks. TierPlan's own agent service can be removed
  only by typing its name, because it cuts TierPlan off from every node the agent runs on.
- **Digest pinning.** Whenever the image of a service changes, or the service is force-updated,
  TierPlan asks the registry for the digest and stores `image:tag@sha256:...` in the spec, the way the
  Docker CLI does. The page shows the tag with the digest as a copyable chip. Every create and update
  carries the registry credential TierPlan holds for that image (see [registries.md](registries.md)),
  so the nodes can pull a private image without a `docker login` on them.
- **Rolling updates.** After a scale-free change the page shows the update as it runs: how many tasks
  are on the new version, Docker's own message, and **Update complete** when it finishes. A paused or
  rolled-back update says so and offers **Roll back** and **Retry update**.

### Services a stack owns

A service that carries a stack label (`com.docker.stack.namespace`) cannot be edited or removed from
its page: the next deploy of the stack would overwrite the change. The page says "Managed by stack
{name}" and the **Edit** and **Remove** buttons say why they are off. **Scale, force update and roll
back stay available**, and each warns that the next stack deploy applies the compose file again.
The server enforces this, not only the page: an edit or remove of a stack's service is refused with
`stack_owned` and audited as a failure. Changing a stack's services through TierPlan comes with stack
deploys in a later release.

## Changing services with host access

A service spec is a whole container specification. A service that binds the Docker socket, adds a
capability, lowers its sandbox or joins the host network is, in effect, root on every node it runs on.
TierPlan treats that in two places, and the second one is the host owner's:

- **On the server**, a person below Admin cannot create or change a service whose spec gives tasks
  host access. The check covers the whole spec, not only the fields the form shows: bind mounts
  outside `/srv`, `/mnt` and `/media` (and the Docker socket in particular), local volumes that
  bind a host path or mount a filesystem other than tmpfs or a network share, added capabilities,
  SELinux, seccomp or AppArmor settings, and the host network. The review step names what needs
  Admin and Apply stays off. A rollback is judged the same way, on the earlier
  version it would restore: rolling back to an earlier version with host access needs Admin, and an
  Operator is told which settings.
- **On the host**, the agent of each manager refuses such a spec whatever TierPlan says, unless the
  host's owner set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` on the agent. Without it, an
  Admin who writes such a spec gets a refusal that TierPlan words as "the manager's agent refused host
  access" (`agent_host_policy`). The same goes for a service mount whose volume has a driver other
  than `local`, for a cluster volume, and for a named volume that already exists on the manager with
  options the volume rule refuses (a host path, or a filesystem other than tmpfs or a network share)
  or a plugin driver: they are refused without the opt-in. A volume made with such options is refused
  at creation (see [containers.md](containers.md)).

**Without that opt-in, TierPlan cannot change a service that already has host access. That includes
scale, force update and roll back.** Every one of them sends the service's whole spec back to Swarm, so
the agent sees the host access in it and refuses the request. Such a service is changed with the
Docker CLI on a manager. TierPlan's own agent service mounts the Docker socket, so it is the usual
example: TierPlan does not scale it, and it is updated by the agent's own self-update (see
[agent-updates.md](agent-updates.md)), which does not go through this check.

To allow TierPlan to manage services with host access, set the variable on the agent of every manager.
On a swarm the agent is a global service, so the setting goes on the service (this command was
not tested for this page; check the variable name and your service name before running it):

```sh
docker service update --env-add TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true tierplan-agent
```

Think first about what this allows. With the opt-in, a compromised TierPlan can run a privileged
service on every node of the swarm, which is root on every node. [agent.md](agent.md#service-specs-and-host-access)
lists exactly what the agent refuses, and [threat-model.md](threat-model.md) explains the risk.
On managers it covers service writes, including the volumes they name; on every agent-managed host it
also covers volume creates. The server's own host is reached through the local socket and has no agent,
so none of this is checked there. A host that is read-only (`TIERPLAN_AGENT_READ_ONLY=true`) refuses
every service change before any of this is looked at.

## The editor

**Edit...** on a service, and **Create service** on the list, open the same editor. It has two tabs,
**Form** and **Spec**, and every change goes through a review before it is sent.

- **Form.** The common settings in cards: the name (fixed once the service exists), the image with a
  tag browser, the mode (Replicated or Global on create, fixed afterwards), replicas, labels,
  environment variables, published ports, secrets, configs, networks, mounts, resources, placement
  constraints and preferences, the restart policy and the update policy. A service that
  has settings the form has no control for shows a caution bar ("This service has additional settings
  not shown here") naming up to three of them, with **Open the Spec tab**. Saving from the form keeps
  those settings as they are.
- **Spec.** The full Docker service spec, as JSON or YAML (a switch remembers your choice). The YAML is
  the same structure in YAML syntax, not a compose snippet, so switching between the two loses nothing.
  A syntax error is shown with its line. The editor does not trap the Tab key, so it can be left with
  the keyboard.
- **Hidden values.** A value you may not see appears as `<hidden>` in the Spec tab and as dots in the
  form. Leave it alone and it stays what it is: TierPlan puts the real value back when it sends the
  spec. Type over it to replace it, or remove the entry to delete it. `<hidden>` cannot itself be a
  value, and a hidden value that points at nothing is refused by name. The same goes for the review:
  a replaced hidden value reads "Hidden value replaced" and prints neither the old nor the new value.
- **Secrets.** Adding a secret the service does not already use, or rolling back to a version that
  uses one, needs the reveal secrets permission (an Admin has it; an Operator only when granted).
  Without it the secret picker offers only the secrets the service uses, and TierPlan refuses the
  change with `secret_reveal_required`. Keeping, removing or re-targeting a secret the service
  already uses stays Operator. A secret is matched by its ID in the swarm, not by the name the spec
  gives it: a reference whose name is not that of the secret its ID points at is refused
  (`spec_invalid`), so correct the name or give only the name. Someone who can change a service can
  still read what that service holds (see residual risk 36 in
  [threat-model.md](threat-model.md)).
- **Review changes.** The editor is locked while TierPlan checks the change. Shows a field-by-field
  list of the current spec against the new one, the image the tag resolves to (with its digest and
  the credential that will be used), anything that needs Admin, and the manager's own notes.
  **Apply changes** stays off, with the reason, when there is nothing to apply, when the spec needs
  a permission you do not hold, or when something it names does not exist yet and has no value.
- **Apply changes.** Sends exactly what the review showed. Runs the steps in order (look up the
  image, create the objects the spec needs, update the service) and then shows the rolling update
  live until **Update complete**. The edit is sent with the version of the service you started from.
  If someone else changed the service meanwhile, nothing is written and the page offers
  **Reload service**.
- **Auto-rollback.** "Roll back automatically if an update fails" is a checkbox in two places that
  are one setting: the Update policy card and the Review step. It is saved on the service
  (`UpdateConfig.FailureAction`): ticked, a failed update rolls back by itself; unticked, a failed
  update pauses. Later updates, from TierPlan or not, follow the same policy.

### Objects the spec names

A spec can name networks, secrets and configs that do not exist yet. The Review step lists them
under **New objects**:

- A missing **network** is created for you as an attachable overlay network.
- A missing **secret or config** has no value Docker could use, so Review asks for one (typed, or from a
  file) and **Apply changes** stays off until each has a value. The form's pickers also offer **Create
  new** for each kind, so you do not have to leave the editor. Creating them needs the same
  permissions as creating them on their own pages (Admin).
- The objects are created first, networks, then secrets, then configs, and the service is updated
  after them.
- **If the service step fails, every object this apply created is removed again**, in reverse order, and
  the message says how many were removed. If one cannot be removed, the message names it and where to
  remove it by hand, so nothing is left behind without you knowing.

### Creating a service

**Create service** (on the list header, and on an empty list) uses the same editor with defaults and the
same review. A name that is already taken is refused before anything is created. A new image is pinned
to its digest like any other. Global services are a choice in the form; jobs are created from the Spec tab. The
permission is `services.manage` (Admin); making a missing secret, config or network along the way needs
the permission for that object as well.

## Tasks and placement

- **Tasks tab.** Lists the tasks that are meant to run, one row per slot, with state, node, the time it
  started and Docker's error. Open a slot to see its earlier tasks: Docker keeps the last few per slot
  (five by default), and the table says how many. **Show only failed** narrows the list. A task whose
  node is not ready reads **Unknown** rather than the state Docker last recorded, because that state
  cannot be confirmed. A task on a node that has left the swarm shows **Removed node**.
- **Placement page.** **Placement** in the sidebar shows where tasks run. **By node** draws one column
  per node (name, role, availability, and its CPU and memory use when **Live stats** is on) with a card
  for each task, coloured by state and grouped by stack. **Grid** draws a table of services against nodes
  with the task count in each cell, shaded to show imbalance. Tasks that no node has taken yet sit in
  **Not placed** with the reason. Both filter by stack and by service name. The service page has a
  **Placement** tab that shows the same view for one service, plus a **Placement rules** card: each
  constraint with how many of the swarm's nodes it matches, and a warning when it matches none.
- **Who.** Anyone who can see the environment's services (`services.read`).
- TierPlan evaluates placement constraints with the same rules as Swarm, including the way Swarm treats
  label names and addresses. Where it cannot tell, it says so instead of guessing.

## Nodes

**Nodes** in the sidebar lists the swarm's nodes: name, status, role, availability, how many tasks run,
the platform, CPU and memory, the engine version, and whether TierPlan's agent on the node is reporting. Badges mark the leader and
the node TierPlan itself runs on. The chips filter to managers, workers, drained and down nodes, and
the search matches the name, the address and any label. A node's page has **Overview**, **Tasks**,
**Labels** and **Inspect**. Anyone who can see the environment (`nodes.read`) can open them.

### Actions and who may run them

All node changes need **Admin** (`nodes.manage`).

| Action | What it does | Warns about |
|--------|--------------|-------------|
| Drain | Swarm stops sending tasks to the node and moves the ones it has | Each task, and whether it moves, stops (global services do not move) or cannot be placed anywhere else |
| Pause | No new tasks; the ones there keep running | |
| Activate | Takes a drained or paused node back | |
| Promote | Makes a worker a manager | |
| Demote | Makes a manager a worker | An even number of managers, a loss of quorum, TierPlan's own node, the only connected manager agent |
| Remove | Removes a node that is down | See below |

- **Warnings, not blocks.** TierPlan tells you what will happen and lets an Admin go ahead. The refusals
  that remain are Docker's own, such as demoting the last manager.
- **Typed confirmation.** Where an action could cut TierPlan off or break the swarm, you must type the
  node's name: draining the node TierPlan runs on or the only manager agent that is connected, and
  demoting a node when the quorum is at risk, when it runs TierPlan, when it holds the only manager
  agent, or when TierPlan cannot read the managers' state. The server checks the name, not only the page:
  without it the request is refused with `confirmation_required` and nothing is sent.
- **Stale pages.** Every change names the version of the node you were looking at. If the node changed
  since, the change is refused ("That changed while you were looking at it") and you reload and decide
  again.
- **Remove...** is offered for a node whose state is **Down** and that is not a manager (demote it
  first). It needs the node's name typed, and Swarm forgets the node: if the machine comes back it has to
  join the swarm again. TierPlan never forces a removal, so a node that is up cannot be removed through
  it. Afterwards TierPlan marks the node's agent as gone (the same as the "revoke" in
  [agent.md](agent.md#revoke-unblock-and-enrol-again) but without blocking it), so a host that rejoins can
  enrol again with the join token. A task that ran on the removed node still shows in history with
  **Removed node**.

### Node labels

The **Labels** tab edits a node's labels in a table. Services use them in placement constraints, such as
`node.labels.disk==ssd`. Before you save, **Effect on services** lists each constraint that mentions a
label you changed, whether the node meets it before and after, and how many tasks Swarm may move. If a
service would stop matching, TierPlan asks **Apply label changes?** and then applies; it does not block.
A label may not start with `com.docker.`, `io.docker.` or `org.dockerproject.`, a node can have at most
64 labels, a name is at most 256 bytes and a value at most 4096 bytes. Below Admin the table is
read-only. Engine labels, set in the node's Docker daemon settings, are shown and cannot be changed
here.

## Secrets and configs

**Secrets** and **Configs** in the sidebar list them with the services that use each, its stack owner,
and its dates. A detail page has Overview, **Used by** (each service, its stack, the file name, UID,
GID and mode) and Inspect. Viewers and above can open them (`secrets.read`, `configs.read`); creating,
rotating and deleting needs **Admin** (`secrets.manage`, `configs.manage`).

| | Secret | Config |
|---|--------|--------|
| Size | 1 to **511,999** bytes | 1 to **1,023,999** bytes |
| Value after saving | **Never shown.** Docker does not return it, so TierPlan cannot show it to anyone, instance admins and holders of **Reveal secrets** included | **Shown** on the detail page and in Inspect, to everyone who can read configs |
| Encrypted by Docker | Yes, in the swarm's store | **No** |
| Delete asks | The name typed | A confirmation |
| Extra at create | | An optional Go template driver |

The size limits are Docker's own (about 500 KB and 1 MB, each less one byte).

- **Creating.** The sheet takes a name (letters, numbers, dot, dash and underscore; up to 64 characters;
  it cannot change later), labels, and the value, typed or from a file. The value field for a secret is
  masked. A config that is valid text fills the text area, so you can see and edit it before saving.
  Neither a secret's nor a config's value is ever written to the audit log, the server's log or the
  database.
- **Configs are not secrets.** Docker stores a config unencrypted and returns it with every list, and
  TierPlan shows it to **everyone who can read configs, which includes a Viewer**. Put passwords, keys
  and tokens in a secret.
- **Used by, and deleting.** The list shows how many services use each object, and the page lists
  them. An object cannot be deleted while any service spec names it, **including a service scaled to 0**.
  Delete is then disabled and says "In use by n services".
- **Rotate: New version...** creates a new object with the new value under the next versioned name
  (`db_password` becomes `db_password_v2`, then `_v3`; you may type another name), copies the labels, and
  points every service that uses the old object at the new one, with the same file name, UID, GID and
  mode. Step 2 shows each service's change before anything is sent. It updates one service at a time, in
  name order, through the same path as an edit, so every service's registry credential, host-access
  check and rolling update apply. It needs both the object's manage permission (Admin) and
  `services.control`. **The first failure stops the run:** the new object, the old one and the services
  not yet reached stay as they were, and the message says how many moved. Services that belong to a
  stack are skipped and listed, because the stack's next deploy would put the old object back. The old
  object is removed after the last service moved, only if "Remove {name} when nothing uses it" is ticked
  and nothing names it any more.
- **Secrets in memory.** A secret's value passes through TierPlan's memory once, while it is being sent
  (see the [threat model](threat-model.md), residual risk 31).

## Service logs

The **Logs** tab of a service shows the log lines of all its tasks together, merged by time, in the same
viewer as a container's logs (see [Logs in containers.md](containers.md#logs)): the last 500 lines and
follow, the **Show** control, **Load earlier lines**, search, timestamps in your time zone or UTC,
wrapping, standard error marked, pausing when you scroll up, **Download** and **Open in new window**.

- **Who.** Everyone who can see the service (`services.read`).
- **One stream.** TierPlan follows the service once, through a manager, and puts the lines in time order
  in a short buffer, so lines from different nodes can arrive slightly out of order by a moment. It is
  not an exact global order: two lines stamped in the same instant have no meaning in order.
- **Task prefixes.** Each line starts with a coloured task name such as `2@node3` (the slot and the
  node), a stable colour per task. **Task names** hides them.
- **Filters.** The **Tasks** and **Nodes** pickers narrow the stream, **Include stopped tasks** adds the
  history, and the filters are in the address. New tasks join the stream by themselves.
- **Markers.** When a task fails or is replaced the viewer inserts a line such as "web.2 stopped: task
  abc12345 failed (exit 1). Waiting for a new task." and updates it when the replacement starts. A node
  that stops reporting while it runs a followed task gets a notice, and another when it returns.
  A reconnect does not replay the markers that happened while it was away.
- **Download** saves a range as a `.log` file, every task merged, each line starting with its task name and
  a UTC time. It is read from Docker once and written afterwards, so a failed read is an error and
  never a half file. A person can run two downloads at a time, and the server four. A file that is
  larger than the server holds in memory says at its top that lines were left out.
- **Shown unmasked, audited, never stored.** The same rules as a container's logs apply: nothing is masked,
  so anything an application prints is visible to everyone who can read the service, and each time a log
  is opened one `logs.viewed` row is written with the service, and each download writes `logs.downloaded`.
  No line of any log is stored or audited.
- **Limits.** The per-person limits of container logs apply (8 live streams, an hour per stream, resuming
  by itself). Access is checked every 15 seconds.

## Stats

Live CPU, memory and network figures are shown for a container, a service, a node and in the lists.

- **Where.** The **Stats** tab on a container and on a service. A **Live stats** switch on the container and
  service lists adds columns for the rows in view. The **Placement** page's node columns show each node's
  total use against its capacity when its **Live stats** is on.
- **Source.** TierPlan asks each node's agent for one reading of each container about every **2 seconds**,
  turns two readings into rates (CPU as a percent of one core, like `docker stats`, memory without the
  file cache, and network and disk rates per second), and sends them to the browser. The agent never holds
  a stats stream open. Two people looking at the same container cost one reading, and a node is asked
  for at most eight at once.
- **Services.** A service's figures are the sum over its running tasks, with a **By task** table. If
  some of its tasks run on nodes that are not reporting, the totals leave them out and say so. A memory
  percentage is shown only when every running task has the same limit; otherwise the tile says "No
  shared limit".
- **History is in the browser only.** The last five minutes (150 points) are kept in the page while it is
  open. **Nothing is stored on the server**, in the database or in the audit log, and a stats reading
  carries counters, not application data, so opening it writes no audit row. Close the page and the
  history is gone. Long-term metrics are not part of this release.
- **Costs.** A Stats tab, and a list with **Live stats** on, each hold one connection open while visible.
  A hidden tab closes it after a minute and reopens it when you return. A person has up to eight such
  aggregate streams (the lists, a service, a node), and a list streams for at most 100 rows. See
  [reverse-proxy.md](reverse-proxy.md) for the proxy settings these streams need.
- **Who.** Anyone who can see the container or service.

## Restart loops

A service that keeps failing is flagged, so a crash loop does not look like a healthy service.

- **The rule.** A service is flagged when **3 or more tasks have failed or been rejected in 10 minutes**.
  Tasks that finish with exit code 0, are shut down by Swarm or are a job's completed tasks do not count.
  A task is counted once. These are the defaults; an environment **Admin** changes them in the
  environment's settings, under **Restart-loop detection** (failed tasks from 2 to 50, minutes from 1 to
  120), and each change is audited.
- **The quiet window.** The flag clears after a **full window with no new failure**, which is 10 minutes with
  the defaults. A flag does not flicker off just because the count dips under the threshold.
- **Where you see it.** A red **Restart loop** badge on the list and the service page (with the latest
  error), an alert on the service page with **View failed tasks**, the **Restart loop** chip, an entry in
  the health bell, and a **Restart loops** tile on the environment's dashboard.
- **Mute.** An Operator can **Mute alerts** for one service that is meant to fail, such as a job. A muted
  service shows a grey **Muted** badge and raises no bell entry. Mute and unmute are audited
  (`restart_loop.muted`, `restart_loop.unmuted`), and the mute is kept in TierPlan's database and travels in
  backups.
- **Containers too.** On any environment, a container with a restart policy that keeps dying with a
  non-zero exit code gets the same badge in the container list and on its page, with the same threshold
  and window, a bell entry and a mute (needs `containers.control`). A policy of "no" is never flagged, and
  the tasks of a service are not tracked as containers, because their service carries the flag.
- **A note on how far back it can see.** Docker keeps only the last few failed tasks per slot (five by
  default) and prunes the rest. TierPlan checks every 15 seconds and remembers the failures it has seen, so
  a loop is met reliably at any threshold up to what Docker keeps, and above that only when the tasks fail
  more slowly than that check. The settings card says so beside the threshold. A restart of TierPlan starts
  counting again from what Docker still holds.

## What is recorded

Each of these is one audit row per attempt on something that exists, with its result and, for a
failure, the reason. A row never holds a secret's or config's value, a masked environment value, or a
line of a log. The audit page filters them by action and by environment.

| Action | Written when |
|--------|--------------|
| `service.scaled` | A scale, with the old and new count. A bulk shares a bulk ID |
| `service.force_updated`, `service.rolled_back` | A force update (with the image and digest) or a rollback |
| `service.updated`, `service.created`, `service.removed` | An edit applied or refused, a service created, a service removed |
| `network.created`, `secret.created`, `config.created` | An object made on its own page, or made by an apply or a rotate (with the source) |
| `network.removed`, `secret.removed`, `config.removed` | An object removed, including by cleanup after a failed apply |
| `secret.rotated`, `config.rotated` | A rotate, with the services moved and skipped |
| `node.availability_changed`, `node.role_changed` | A drain, pause, activate, promote or demote, with the warnings shown |
| `node.labels_changed`, `node.removed` | Labels saved, a node removed (the agent revoke is its own `agent.revoked` row) |
| `restart_loop.muted`, `restart_loop.unmuted` | A mute or unmute |
| `environment.restart_loop_settings_changed` | The threshold or window changed, with the old and new values |
| `logs.viewed`, `logs.downloaded` | A service log opened or downloaded |
| `permission.denied` | A change refused for lack of permission |

A host-access refusal from the agent, a stale version and a cleanup that could not remove an object are
failures on the row of the action that met them.
