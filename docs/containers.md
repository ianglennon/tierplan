<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Bayplan Authors -->

# Containers, logs, shells, networks, volumes and events

This page explains what Bayplan does with the Docker objects on an environment (a local
socket host, a standalone host with an agent, or a swarm), who may do it, and what is
written down about it. It also states two behaviours plainly, because they are decisions
and not oversights: [logs are shown unmasked](#logs) and [a process started in a browser
shell can keep running after the shell is closed](#closing-a-shell-does-not-stop-what-you-started-in-it).

The exact list of what each role may do is in [permissions.md](permissions.md), which is
generated from the code. [access.md](access.md) explains roles and extra permissions.
[agent.md](agent.md) explains what the agent on a remote host will and will not forward.
Swarm services, nodes, secrets and configs are in [services.md](services.md).

Every change on this page goes through a typed route, not through a raw Docker proxy, and
is checked against the person's role on that environment first. Nothing here is retried: if
the connection to a host is lost while Bayplan is changing something, the answer says the
host did not answer, and you look at the host before you try again.

## Containers

### The list

- **Grouping.** Containers sit under their Compose project or swarm stack, taken from the
  `com.docker.compose.project` and `com.docker.stack.namespace` labels. Containers with
  neither go under **Ungrouped** at the bottom. **Group by** also offers **None**, and
  **Node** on a swarm. The choice, the search and the filters are in the address, so you can
  share a link.
- **Search and filters.** The search box matches the name, image, project, stack, service
  and every label key and value. The server does this before it cuts the list, so a match
  is found even on a host with more containers than the list shows. The status chips are
  Running, Exited, Paused, Restarting and Unhealthy, and their numbers do not shrink as you
  type in the search box. On a swarm there is also a node filter and a note when some nodes
  are not reporting.
- **Large hosts.** The list shows at most 500 rows and says so under the table. Narrow it
  with the search or a filter to see the rest.
- **Live updates.** Rows change in place when Docker reports a change. While a menu or
  dialog is open or rows are selected, the list holds still, and a **changes waiting**
  pill applies them when you are ready.
- **Live stats.** The **Live stats** switch adds CPU, Memory and Network columns for the
  rows you can see (see [Stats](#stats)). It is off until you turn it on, and it is in the
  address, so a shared link carries it.
- **Restart loops.** A container with a restart policy that keeps dying gets a red **Restart
  loop** badge (see [Restart loops](#restart-loops)).

### The container page

The page has five tabs: **Overview** (state, health, image, command, ports, mounts, networks,
restart policy, labels and environment variables), **Logs**, **Stats**, **Shell** and **Inspect** (the
engine's JSON). The values of environment variables, and of the options of the container's
log driver, are hidden for everyone who does not hold the **Reveal secrets** extra
permission, in the Overview and in the Inspect JSON alike. Labels are shown as they are.
Do not put a secret in a label.

### Actions and who may run them

| Action | Needs | Asks first |
|--------|-------|------------|
| Start, restart, pause, unpause | Operator (`containers.control`) | No, it runs at once |
| Stop, kill | Operator (`containers.control`) | Yes, a dialog listing every target and its node |
| Remove | Operator (`containers.remove`) | Yes, the same dialog |

A Viewer sees the buttons disabled, with the reason ("Needs Operator").

- **Stop** uses the container's own stop signal and timeout. When you act on one container
  the dialog has an **Advanced** disclosure where you can set a timeout (up to 86400
  seconds) or a signal for that stop. Bulk actions always use the container's own settings.
- **Kill** sends SIGKILL at once. The process gets no chance to shut down cleanly.
- **Remove** has two checkboxes, both off. **Force (stop if running)** is required to remove
  a container that is running, paused or restarting. **Also remove anonymous volumes** runs
  the removal with Docker's `-v`. Named volumes are never touched. Because that deletes
  data, ticking it also needs the **Delete volumes** extra permission, and the server asks
  for it again: an Operator without it gets a refusal and a `permission.denied` audit row,
  and nothing is removed.
- **A swarm task or a Compose project container.** You may stop, kill or remove one, and
  the dialog warns you first. For a task of a service it says "Swarm will replace it. To stop
  it for good, scale the service." For a Compose project container it says "The next deploy
  will recreate it."

### Bulk actions

Tick rows (Shift selects a range, and a group checkbox selects the group) and use the bar
that appears. Start, stop, restart, kill, pause, unpause and remove all work in bulk.

- Selections of more than 25 are sent in chunks of 25 that share one **bulk ID**. The server
  runs up to 16 targets at once and no more than 4 per node.
- A container that fails or is refused never stops the others. Each row shows its
  transitional state ("Stopping...") until Docker confirms it, and a toast sums up
  ("Stopped 28, 2 failed") with the reason for each failure in the results dialog.
- **Audit.** Every container gets its own audit row (`container.started`,
  `container.stopped`, `container.restarted`, `container.killed`, `container.paused`,
  `container.unpaused`, `container.removed`) with its result, failures included. Rows from
  one bulk share the bulk ID. The results dialog has **View in audit log**, which opens the
  audit page with a bulk filter chip you can remove.

### Hosts that refuse, and agents that are too old

- A host whose owner set `BAYPLAN_AGENT_READ_ONLY=true` refuses every change, and Bayplan
  says so by name before it sends anything. Lists and logs still work. See
  [agent.md](agent.md#settings-on-the-host-read-only-and-self-update).
- An agent from before Phase 4 refuses actions, shells, networks and volumes with
  `agent_policy_denied`. Bayplan then tells you the agent needs an update, and updates it
  itself unless the host turned self-update off. See [agent-updates.md](agent-updates.md).

## Bayplan's own containers

Bayplan will not make it easy to cut itself off from a host. The Bayplan server's container
and every Bayplan agent container (found by the agent's labels, confirmed by a fresh
inspect at the moment of the request, never by the cached list) are treated specially:

- A lock badge marks them in the list.
- **Stop, kill, restart, pause and remove** need you to **type the container's name**. The
  dialog warns that this cuts Bayplan off from the host. The server enforces this, not only
  the page: a request without the right name is refused with `confirmation_required`, and
  nothing is sent. Start and unpause need no confirmation.
- They are **never part of a bulk action**. Select-all skips them, and a bulk request that
  names one reports it as skipped. You act on them one at a time.
- **No shell can be opened in them.** The Shell tab is unavailable, with the reason.
- The attempt is written to the audit log before the request is sent. If that row cannot be
  written, the action is not sent.

## Logs

- **Who can read them.** Everyone who can see the container: a **Viewer** and above
  (`containers.read`).
- **Shown unmasked.** Log lines are shown exactly as the application wrote them. Bayplan
  does not look for secrets in them, so log lines are **never masked**, and anything an application
  prints is visible to everyone who can read that container's logs. If an application logs
  a password, a token or a personal address, every Viewer on the environment can read it.
  Fix that in the application, or do not give people access to that environment. This is
  deliberate: a log that is masked by guesswork hides the line you were looking for.
- **Audited, never stored.** Every time someone opens a log, one `logs.viewed` row is
  written with the container, the node and what was asked for. A browser that reconnects
  keeps the same viewer ID and writes no second row. Every download writes a
  `logs.downloaded` row with its range. No line of any log is stored, audited or written to
  Bayplan's own log.
- **Reading.** The viewer opens on the last 500 lines and follows the log. **Show** offers
  the last 500 lines, 5 minutes, an hour, 24 hours, all, or a custom range. Scrolling up
  pauses following, and a **Jump to latest** pill resumes it while lines keep arriving.
  **Load earlier lines** pages back. Search works on the lines already loaded (plain text,
  regular expression, match case, and a toggle to show only matching lines). There are
  toggles for wrapping and for timestamps, shown in your browser's time zone or in UTC.
  Standard error lines are tinted and marked, and ANSI colours are drawn. The viewer keeps
  at most 50,000 lines and says so when it drops older ones.
- **Restarts and stops.** When the container restarts, a divider marks it and the viewer
  follows the new run. When it stops, the viewer says so and waits for it to start again.
- **Download.** **Download** saves any range as a `.log` file, straight from Docker, with
  timestamps if you ask. It is not limited by what your browser holds. A person can run two
  downloads at a time.
- **Limits.** A person can have 8 live log streams open (the ninth is refused), and a stream
  is ended after an hour, and your browser reconnects by itself and resumes from the last line it
  has. If the host's
  agent is already serving as many logs and shells as it can (32 at once), Bayplan answers
  "Close one and try again".
- **Open in new window.** The viewer can open in its own window without the app around it.
  It is behind the same sign-in and permission as the tab.
- **Drivers Docker cannot read back.** Docker can only return logs from some log drivers.
  If Docker answers that it cannot read this container's log (its own refusal, not a guess
  from the driver's name), the Logs tab says so and names the driver and its options. The
  values of the options are hidden for anyone without **Reveal secrets**.
- Access is checked again every 15 seconds. A person who loses the permission or signs out
  has the stream closed, and the page says their access changed.

## Stats

The **Stats** tab of a container shows its CPU, memory, network and disk use live: four tiles with
their trend, and four charts.

- **Who can open it.** Everyone who can see the container (`containers.read`), a Viewer and above.
- **Where the numbers come from.** About every **2 seconds** Bayplan asks the node that runs the
  container, through its agent, for one reading, and turns two readings into rates. CPU is a percent
  of one core, as `docker stats` shows it, so a container using two cores reads about 200%. Memory is
  what the container uses without the file cache, shown against the container's own limit when it
  has one (and "No limit set" when it has not). The agent never holds a stats stream open; it answers
  one reading at a time.
- **History lives in your browser.** The last five minutes are kept in the page while it is open.
  **Nothing is stored on the server**, in the database, in the audit log or in Bayplan's log, and
  closing the page loses the history. Opening the tab writes no audit row, because a reading holds
  counters and nothing the application wrote.
- **States.** The tab says when the container is not running (and starts again by itself when it
  does), when the node is not reporting, and when the view has paused because the browser tab was
  hidden for a minute. A gap on the chart marks the pause. If the host's owner turned the agent
  read-only, stats still work, because they are reads.
- **Cost.** An open tab holds one connection and one reading per 2 seconds for this container.
  Two people looking at the same container cost one reading. A person can have 8 stats streams
  open at a time across the container tab, the lists and the service pages. See
  [reverse-proxy.md](reverse-proxy.md) for the proxy settings these streams need.

Services and nodes have the same figures summed over their tasks, described in
[services.md](services.md#stats).

## Restart loops

A container whose restart policy keeps bringing it back after it fails is a restart loop, and it is
easy to miss because the container is "Running" most of the time.

- **The rule.** Bayplan counts the times a container died with a **non-zero exit code**. At **3 deaths
  in 10 minutes** (the defaults) it is flagged, and the flag clears after a full window, 10 minutes,
  with no new death. An environment **Admin** changes both numbers in the environment's settings,
  under **Restart-loop detection**. A container with the restart policy `no` is never flagged, and
  neither is a task of a swarm service, because its service carries the flag (see
  [services.md](services.md#restart-loops)).
- **Where you see it.** A red **Restart loop** badge in the list and on the container's page, with
  the count, the window and the last exit code in its tooltip, and an entry in the health bell. Two containers with the same name on different nodes are two
  containers.
- **Mute.** An Operator (`containers.control`) can **Mute restart-loop alerts** from the row menu or
  the page's More menu for a container that is meant to fail. A muted container shows a grey **Muted**
  badge and raises no bell entry. Mute and unmute are audited (`restart_loop.muted` and
  `restart_loop.unmuted`). A mute is kept when the container is recreated under the same name.
- **What it cannot see.** Bayplan sees the deaths the host reports while it is connected and checks
  every 15 seconds. A host that was unreachable shows no deaths for that time.

## Shells

A shell is a terminal in the browser, running inside a container, started with Docker's exec.

- **Who can open one.** Only people who hold the **Container shell** permission
  (`containers.exec`): an Admin always, an Operator only when the assignment carries it, a
  Viewer never. It is a dangerous permission. A shell is root-equivalent on the container
  at least, and often on the data it holds.
- **Opening one.** The Shell tab has a launcher. **Automatic** runs bash if the image has
  it and sh otherwise, in one request. You may pick bash, sh or ash, or **Custom command**
  (split into words the way a shell reads quotes and backslashes, and never given to a
  shell, so nothing in it is expanded). **Options** sets the **User** to run as (for example
  `root`, `0` or `www-data`) and the **Working directory**. Left empty, the container's own
  user and directory are used. There are no extra environment variables, no privileged exec,
  and no attach to the container's main process.
- **The choice is remembered.** The last choice you made for an image is kept for you, and
  only you, in Bayplan's database. A custom command is kept in clear text, and travels in
  backups. The audit row records the command anyway.
- **Not possible.** The container must be running (the tab offers **Start container** if you
  may start it). An image without the shell you asked for says "No shell in this image" and
  offers the custom command field. A read-only host refuses shells. Bayplan's own containers
  are refused. An agent that is too old to run an exec needs updating first.
- **The idle timeout and the cap.** A shell closes after **30 minutes** with no input and no
  output, and warns one minute before; pressing a key keeps it open. A person can hold **10**
  shells at once, and the eleventh is refused with the limit named. Both are instance
  settings: an **instance admin** sets them in **Settings, General, Container shells**
  (idle from 5 to 240 minutes, shells per person from 1 to 50). A change is a
  `settings.updated` row in the audit log. There is no maximum lifetime.
- **Losing access.** A shell does not outlive its person's sign-in, their grant on the
  environment, or the permission. Bayplan checks every 5 seconds, and closes the shell with
  "Your access changed".
- **Your shells.** The shell tab shows **Your shells (n)** when you have shells open, where
  you can end any of them.
- **Active shells.** An environment **Admin** (`shells.manage`) sees every open shell on the
  environment in **Settings, Environments**, the environment's **Shells** tab: the person,
  the container, the node, when it opened and how long it has sat unused. **End session**
  closes one at once. Ending someone's shell writes a `shell.ended` row naming the owner.
- **What is recorded.** `shell.opened` records the person, the container, the node, the
  command, the user, the working directory and the client address. `shell.closed` records
  how long it lasted, the exit code (or that the process was **still running**), and why it
  ended: `exited`, `disconnected`, `idle`, `access_lost`, `ended_by_user`, `ended_by_admin`,
  `expired` or `server_shutdown`. **What you type and what is printed is never recorded.**
- **The terminal.** Copy and paste use Ctrl+Shift+C and Ctrl+Shift+V (Cmd on a Mac), search
  Ctrl+Shift+F, and Ctrl+Shift+Home moves focus to the toolbar. Every other key goes to the
  shell. The toolbar sets the font size and a screen reader mode, which the browser keeps in
  its local storage under `bayplan-terminal-font-size` and `bayplan-terminal-screen-reader`.
  The terminal follows the light or dark theme. **Open in new window** shows the terminal
  alone, behind the same sign-in, with its own session.

### Closing a shell does not stop what you started in it

When you close the tab, lose your connection, or the shell is closed for being idle, Bayplan
closes the terminal. A process that you started in the shell, and that does not exit when its
terminal goes away, **can keep running inside the container**. A program started with `nohup`
or `&` and then disowned, a daemon, or anything that ignores the hang-up signal will not stop
by itself. Docker does not end such a process when the connection to it closes.

This is a known limitation, which was looked at and accepted for this release rather than
worked around. Bayplan does not search for or kill what is left. Sessions also cannot be
re-attached: **Reconnect** starts a new, separate shell.

To check a container for a process you left behind, on the host that runs it:

```sh
docker top <container>
docker exec <container> ps
```

`docker top` lists the processes from the host's side, so its PID column holds the host's
numbers. `docker exec <container> ps` (when the image has `ps`) shows the numbers inside the
container. To stop one, use the number inside the container:

```sh
docker exec <container> kill <pid>
```

(from the host, `kill` on the number `docker top` shows also works if you are root there).
These commands were tried on an Alpine test container; they need `ps` and `kill` in the image,
and a process that ignores `kill` needs `kill -9`.

The `shell.closed` row in the audit log records `still_running` when Bayplan did not see the
shell's process end, which is where to look. Ending a shell as an administrator has the same
limit: the process the person left running keeps running.

## Networks

- **The list** shows each network's driver, scope (Swarm or Local), subnet, and what is
  attached (containers and services), and who owns it by its stack label. Search covers the
  name, driver, subnet and owner. A swarm-scoped network (an overlay) appears once. A network
  that exists on each node separately has one row per node.
- **Create network** needs **Admin** (`networks.manage`). The driver is bridge, overlay,
  macvlan or ipvlan, and the form defaults to overlay on a swarm and bridge on a standalone
  host. Attachable, internal and labels are common fields. **Advanced** holds the subnet,
  gateway, IP range, driver options and IPv6. An overlay is created through one swarm
  manager. A local driver on a swarm is created on the node you choose.
- **The overlap warning.** When you type a subnet, Bayplan compares it to the networks it can
  see and warns if it overlaps one. The warning does not block Create: Docker decides. It
  does not suggest a free subnet.
- **In use.** A network cannot be deleted while a container is attached, a stopped container
  included, or while a **swarm service's spec mentions it, even with no running tasks** (a
  service scaled to zero still blocks). Delete is disabled and lists up to five users by
  name. Just before it deletes, Bayplan reads every node again. If a node does not answer, it
  refuses with "the node did not answer" rather than guess.
- **Built-in networks** (`bridge`, `host`, `none`, `ingress`, `docker_gwbridge`) can never be
  deleted from Bayplan. An unused network deletes after a simple confirmation. Each create and
  each delete is an audit row (`network.created`, `network.removed`).
- **The network page** has Overview, **Attachments** (each container with its node, addresses
  and aliases, and each service) and Inspect.
- **Connect and disconnect.** An **Operator** (`networks.connect`) can connect a container to
  a network, with an optional alias and addresses, and disconnect it, from the network's
  Attachments tab or the container's Overview. Disconnect asks first and is never forced.
  Both are audited (`network.connected`, `network.disconnected`). Docker's own refusal is
  shown (for example, a local network on another node or a non-attachable overlay).

## Volumes

- **The list** shows each volume with the node it is on (on a swarm, each node's copy is its
  own row, and Delete acts on that node's copy only), what uses it, a mark on anonymous
  volumes, and a stack or project group. The **usage** filter shows used, unused or all.
- **Create volume** needs **Admin** (`volumes.create`). The Type select offers **NFS**,
  **SMB (CIFS)** and **Host folder**, which fill in Docker's local driver options, and shows
  the options that will be sent. On a swarm you pick the node, because volumes live on one
  node. Bayplan checks the name on that node first, because Docker silently reuses an
  existing volume of the same name.
- **The SMB password is stored in the volume's options.** Docker keeps the share's password
  among the volume's driver options, in clear. Anyone who can inspect the volume on the host
  can read it. Bayplan writes no password into its audit log, and hides the values of a
  volume's options from everyone without **Reveal secrets**, in the volume page and its
  Inspect JSON. If you can, use a credentials file instead of a password.
- **A Host folder volume points at a directory on the host**, which must already exist. It
  is a bind of that path. Only an Admin may create one, and the options are in the audit log.
  Bayplan cannot create containers, so it cannot mount it, but anyone who starts a container
  with that volume sees the folder. See [agent.md](agent.md#what-the-agent-forwards).
- **In use.** A volume is in use while any container mounts it, a **stopped container
  included**, or while a swarm service's spec mounts it, even at zero replicas. A bind mount
  that happens to be spelled like the volume is not a use. Delete is disabled, with the users
  listed.
- **Delete** needs the **Delete volumes** extra permission (`volumes.delete`), and you type
  the volume's name to confirm. Select several unused volumes to delete them together: you
  type `delete 3 volumes` (with your count) instead. Up to 25 go in one request, and a
  longer selection is sent in chunks that share one bulk ID. Each volume is checked again on
  its node just before it is removed, never forced, and written as a `volume.removed` row.
- **Sizes on demand.** The list does not show sizes, because Docker's disk usage report is
  slow. **Calculate sizes** asks each node, a few at a time, and fills the Size column. A
  node that does not answer shows a dash with the reason. Nothing else asks for sizes.
- **The volume page** has Overview (details, the **Used by** list with the mount point and
  whether it is read-only, labels and the options) and Inspect.

## Events

The **Events** page (**Resources, Events** in the sidebar) shows what Docker reports on the
environment: containers, images, networks, volumes, and on a swarm services, nodes, secrets
and configs.

- **Who can open it.** Anyone who can see the environment (`environments.read`), which is a
  Viewer and above.
- **Filters.** Show events from the last 5 minutes, an hour or 24 hours. Filter by type,
  action, node and name, search by name, image or attribute, and **Pause** to stop the list
  moving while you read. A row expands to show its attributes. Clicking a name opens that
  container, network or volume. The filters are in the address, so a shared link carries your
  search text.
- **Docker's short history.** Bayplan shows what Docker reports, as Docker reports it. Docker
  keeps only a small buffer of events and none across a restart of the Docker daemon, so
  older history is not available. Bayplan keeps up to 1000 events from the last 24
  hours in memory, so a restart of Bayplan loses them.
- **Nothing is stored.** Events are not written to the database, to your browser's storage, or
  to the audit log. They are not matched to the Bayplan user who caused them.
- **What a row can show.** Only a fixed set of attributes: the name, image, exit code, signal,
  type, container, driver, reason, mount destination, read or write, the Compose project and
  the stack namespace. No other label, environment variable or command line ever appears.
- **Lists follow events.** The container, network and volume lists refresh on their own when
  an event arrives, and are read again from time to time, because Docker does not promise to
  deliver every event.
