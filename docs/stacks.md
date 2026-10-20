<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Bayplan Authors -->

# Compose stacks

A stack is a compose file that Bayplan deploys and keeps tidy: its services, networks, secrets and configs go
together, and nothing is left behind when you remove it. This page explains how to write one, what the review
shows before anything changes, what a deploy does on a swarm and on a standalone host, how it stops, starts and
goes away again, and what Bayplan writes on your hosts. It also says plainly what Phase 7 does not do, and a few
behaviours that are open decisions, not oversights.

The workflow is always the same: **edit, save, review, deploy**. Nothing is sent to any engine until you press
**Deploy stack** on the review, and the review shows what that will change on the environment as it is now.

Every stack is made for one kind of environment and stays on it: a **swarm** stack goes to a Docker Swarm through
one of its managers, a **standalone** stack goes to a single Docker host (the one Bayplan runs on, through its
socket, or a remote one through its agent). The kind is fixed when the stack is created, from the environment.

The exact list of what each role may do is in [permissions.md](permissions.md), which is generated from the code.
[agent.md](agent.md) explains what the agent on a remote host will and will not forward, and
[threat-model.md](threat-model.md) says what stacks add to the risks of running Bayplan.

- [Create a stack](#create-a-stack)
- [The editor](#the-editor)
- [Stack files](#stack-files)
- [Variables](#variables)
- [Secrets and configs](#secrets-and-configs)
- [What a stack cannot use](#what-a-stack-cannot-use)
- [A worked example](#a-worked-example)
- [The review](#the-review)
- [Image pinning and Update images](#image-pinning-and-update-images)
- [Deploying](#deploying)
- [When a deploy fails, is cancelled or is interrupted](#when-a-deploy-fails-is-cancelled-or-is-interrupted)
- [Stop, Start and Remove](#stop-start-and-remove)
- [One operation at a time](#one-operation-at-a-time)
- [What Bayplan writes on your hosts](#what-bayplan-writes-on-your-hosts)
- [Host access and who may do what](#host-access-and-who-may-do-what)
- [The other tabs and the links from other pages](#the-other-tabs-and-the-links-from-other-pages)
- [Limits](#limits)
- [What is recorded](#what-is-recorded)
- [Behaviour that is still open](#behaviour-that-is-still-open)
- [What Phase 7 does not do](#what-phase-7-does-not-do)

## Create a stack

**Stacks** is the first item in each environment's sidebar, and **All stacks** lists the stacks of every
environment you may read, grouped by environment, with search, status chips and sorting. Creating a stack needs
the Admin role on the environment (`stacks.create`).

**Create stack** asks for a name and a starting point (the starter example, an empty file, or a compose file you
upload). The name is lowercase letters, numbers, dash or underscore, starts with a letter or number, is at most
63 characters, and **can't change later**: it is the swarm namespace or the Compose project name on the host.
Bayplan checks it as you type.

A stack whose name a stack on that environment already has, that Bayplan does not manage, is refused: *a stack
named X already exists on this environment, not managed by Bayplan*. Bayplan never silently takes over a stack
(see [what Phase 7 does not do](#what-phase-7-does-not-do)). The same check runs again at the review and at the
deploy.

Creating a stack sends nothing to any engine. It reads **Not deployed** until you deploy it.

## The editor

A stack page has four tabs: **Overview**, **Editor**, **Resources** and **Logs**. The Editor has three panes:

- **Files**: `compose.yaml` and any other [stack files](#stack-files), in a code editor with YAML highlighting.
  Errors are marked on their line and listed under **Problems**, each with its file and line; pressing one jumps
  to it. Typing `${` offers the names of the variables the stack has.
- **Variables**: the [variables table](#variables).
- **Rendered**: the stack as it will be deployed, with variables filled in, short-form ports, every image as the
  last deployed `tag@sha256:...` (or as written, with a "will be pinned" comment) and hidden values shown as
  `<hidden>`. It is read only. **Bayplan never changes your compose file**: pins and rendering are Bayplan's own
  state, shown beside your file.

**There is no autosave.** Your edits are kept in the editor until you press **Save draft** (or Ctrl+S, or
Cmd+S). The saved draft belongs to the stack, not to you: it is shared, and the page shows who saved it last and
when. Two people can therefore collide. If someone saves while you are editing, you are told, and may reload
their version or save over it. **Discard draft** deletes the saved draft and goes back to what is deployed.
Leaving the page with unsaved edits asks you first. **Review and deploy** saves first, then opens the review.

Errors **block** a deploy and warnings do not. The blocking ones are invalid YAML or schema, a variable with no
value and no default, a relative bind mount, a stack file that is missing or has an illegal path, and anything the
host-access rules refuse (see [Host access](#host-access-and-who-may-do-what)). The warnings are keys the target
ignores, an image with no tag or `:latest`, an image no registry has, and a variable nothing uses. The lint knows
the target: a key that Swarm ignores and a standalone host honours (or the other way round) is a warning only on
the target that ignores it.

## Stack files

A stack holds extra files beside `compose.yaml`: an `.env`, a `nginx.conf`, a secret. They are stored **in
Bayplan, as part of the stack**, and edited in the same editor. Add them with **Add file** (or **Upload files**).

An `env_file:`, a `secrets: ... file:` and a `configs: ... file:` in the compose file are resolved **only against
these files**. Nothing is ever read from the Bayplan server's own file system, so a compose file cannot make
Bayplan read `/etc/passwd`, `/data` or anything else on the machine it runs on, and the path rules below have no
file system to escape into.

A path is relative, written with `/`, and made of plain names:

- no leading or doubled `/`, no `.` or `..` name, no backslash, no colon;
- no control, format or line-separator character (so a right-to-left override cannot make one name look like
  another), valid UTF-8, at most 255 characters in all and 100 in one name;
- no two files of the same name.

A stack holds at most 50 files (`compose.yaml` included), 1 MiB per file and 8 MiB in all. A compose file or a
stack file over its limit is refused when you save it or upload it. A file the compose file does not refer to is
marked **Not used**, and a file that is a secret is marked **Secret**.

## Variables

**Variables come only from the stack's own table.** The caption of the table says it exactly: *Values come from
this table. A variable with no value here uses the default written in the compose file, like `${NAME:-default}`.
Bayplan fills them in itself before it deploys.*

So the precedence is: **the stack's variables table, then the default written in the YAML**. There is **no
environment-level or global variable** in this version, and **the Bayplan server's own environment is never
read**: a `${HOME}` or `${BAYPLAN_...}` in your file is a variable like any other, and without a value in the table
and a default in the file it is an error, not the server's value. (Compose's own `.env` convention is replaced by
the table: a stack file called `.env` that nothing uses is only warned about, *`.env` isn't read automatically.
Import it into Variables, or use it as an env_file.*)

- Every `${VAR}` in `compose.yaml` appears as a row, red when it is required and has no value, or showing the
  YAML default as its placeholder. Only `compose.yaml` is scanned: a `${...}` inside a configuration file the
  stack carries is that file's own text.
- A variable in the table that nothing uses is marked **Unused** and warned about, and does not block.
- **Import .env** reads pasted or chosen `KEY=VALUE` text into rows, merging or replacing. **Bulk edit as text**
  edits the whole table as `NAME=value` lines. Nothing is saved until you save the draft.
- A table entry with no value counts as no value: `${NAME:-default}` uses its default and a plain `${NAME}` is an
  error.

**A secret variable** is stored encrypted, is shown as `••••` in the table and as `<hidden>` in the review, the
rendered YAML, the console, the logs and the audit log, and is readable only by someone who holds
`secrets.reveal`. **Show** reads it for 60 seconds and writes a `stack.value_revealed` row first; if that row
can't be written, you get no value. A secret value needs at least 4 characters, so that hiding it can't hide
ordinary words. Secret variables are interpolated like any other: they are **not** turned into Docker secrets.
Put a value a container should read as a file in a [secret file](#secrets-and-configs) instead.

## Secrets and configs

Declare a secret or config in `compose.yaml` with `file:` (a [stack file](#stack-files)), `environment:` or
`content:`, as you would for any compose file. Bayplan only creates the secrets and configs that at least one
service uses, and an `external: true` one is never created, changed or removed (it must exist, or the deploy
stops before anything changes).

- **A file referenced under `secrets:` is a secret file.** The server seals it with the stack's other secrets, it
  shows as hidden in the editor (**The content of tok.txt is hidden**), it is replaced by typing or uploading new
  content and is never shown again except through the audited, permissioned **Show content** (60 seconds,
  `secrets.reveal`). A secret file can be at most 500 KB on a swarm (a swarm refuses more) and 1,000 KB on a
  standalone host; a config can be at most 1 MB on a swarm and 1,000 KB on a standalone host.
- **On a swarm**, the secret or config is created as a swarm object named `<stack>_<name>_<hash>`, where `<hash>`
  is the first 8 hex digits of a SHA-256 over the name and the content. **A changed file gives a new object**: the
  services move to it in the same deploy, and the old object is removed after a deploy that succeeded. The review
  says *new version*. An unchanged file gives the same name, so a redeploy nobody edited changes nothing.
- **On a standalone host**, there are no Docker secrets. Bayplan writes the content into the container as a file
  just before it starts, at the `target` you gave (by default `/run/secrets/<name>` for a secret), and a changed
  secret or config recreates the container. The file is owned by the user the container starts as: **`uid`
  and `gid` keys on a standalone secret or config are refused**, as are a `read_only` service that uses one
  (the file can't be written into it), a path too long to write, and an `external` secret or config (there is no
  Docker object to read).
- A secret or config that sets `driver`, `driver_opts` or `template_driver` is refused on a swarm, because
  creating it without them would change what a service reads.

Docker returns a swarm **config**'s content with every inspect and does not encrypt it, so put credentials in a
secret, never in a config (see [services.md](services.md#secrets-and-configs)).

## What a stack cannot use

**Relative bind mounts (`./data:/data`) are a blocking error on both targets.** A stack has no directory on the
host for a path to be relative to: its files are rows in Bayplan, not a folder. The error says *use a named
volume, or an absolute path on the host*. What to use instead:

- a **named volume** (`web-data:/var/lib/data`) for data a container writes;
- a **stack file as a config or a secret** (`configs: ... file: ./nginx.conf`) for a file a container reads: see
  the [worked example](#a-worked-example);
- an **absolute host path** (`/srv/app:/data`), which is host access: an Operator may use a path under `/srv`,
  `/mnt` or `/media`, anything else needs Admin, and a remote host's agent also refuses anything outside those
  roots unless the host owner opted in (see [agent.md](agent.md)).

Also refused or ignored:

- **`include` is not supported**: *put everything in compose.yaml*. A remote `include` would make the Bayplan
  server fetch a URL, and a local one has no directory to read from.
- **`extends` is not supported**: *copy the settings you need into the service*.
- **`build` is ignored**: Bayplan does not build images, it deploys the image named in `image`. A service with
  `build` and no `image` is an error on both targets.
- **`label_file`**, and on a swarm the keys `docker stack deploy` also refuses (`volumes_from`, `volume_driver`,
  `cpu_quota`, `cpu_shares`, `cpuset`, `mem_limit`, `memswap_limit`) are blocking errors that name the key.
  Standalone-only or swarm-only keys the other target ignores are warnings.
- On a standalone host `provider` services and `use_api_socket` are refused (the first would run a program on the
  Bayplan server, the second is a mount of the Docker socket that skips the host-access rules).

`env_file:` is supported: Bayplan reads the named stack file once, with Compose's own `.env` parser, so a swarm and
a standalone host can't see different values.

## A worked example

This stack serves a page behind a proxy on a port you choose, with its configuration as a stack file and its data
in a named volume. There is no relative bind mount: the nginx configuration is a **config whose content is a stack
file**.

`compose.yaml`:

```yaml
services:
  web:
    image: nginx:1.27
    ports:
      - "${WEB_PORT:-8080}:80"
    configs:
      - source: site
        target: /etc/nginx/conf.d/default.conf
    volumes:
      - web-data:/usr/share/nginx/html

configs:
  site:
    file: ./site.conf

volumes:
  web-data:
```

`site.conf` (a second stack file, added with **Add file**):

```nginx
server {
  listen 80;
  location / { root /usr/share/nginx/html; index index.html; }
}
```

Open **Variables**: `WEB_PORT` is listed with `8080` as its placeholder, from the YAML default. Type `18080` to
override it. Press **Review and deploy**: on a first deploy the review says *First deploy*, lists the service as
**Added**, the network as added and the image as *Will be pinned to sha256:...*.

## The review

**Review and deploy** saves the draft and opens a review that checks the stack, looks up any image that needs a
fresh digest and compares the result with **what is running now**, not with an earlier draft. It has:

- a **Summary** of services, networks, volumes, secrets and configs added, changed and removed, and of images
  (pinned and new);
- an **Images** table: each service's image as *Pinned (running)*, or *New* with the digest it will be pinned to;
- **Changes**: every changed service expands to a field-by-field diff, with secret and hidden values shown only as
  *Changed (hidden)* at their path, and the services a recreate carries with it named (a service that shares the
  network, processes or volumes of a recreated one is recreated with it, and one with `depends_on: restart: true`
  is restarted);
- **Other resources**: networks, volumes, secrets and configs added, changed, removed or external;
- **Rendered YAML**: a text diff of the rendered stack **against the last deploy**.

The field diff and the Rendered YAML answer different questions, and both are labelled. **The field diff compares
with what is running on both targets**: on a swarm it reads each service's current spec, and on a standalone host
it rebuilds every running container's settings. A container changed by hand on the host (`docker update`, for
instance) is therefore listed as changed at that setting even though your compose file did not change, and the
next deploy recreates it. **The Rendered YAML tab compares with the last deploy Bayplan made**, so it shows what
you changed in the file, whatever someone did on the host.

**Errors stop the deploy** and are listed with a link to the line; **Deploy stack** says why it is off.
**Warnings do not.** A review of a stack that is the same as what runs says *No changes* and has nothing to deploy.

**A concurrent-deploy warning.** A draft remembers the deployed version it started from. If someone deployed
since, the review shows *This stack was deployed after you started editing* and a **See what changed** view
(what the draft started from, what is deployed now, your changes, secrets hidden), and **Deploy stack** stays off
until you tick that you have seen it. The server enforces this as well as the page. Bayplan never overwrites
someone's deploy silently.

A review right after a successful deploy is empty on both targets.

## Image pinning and Update images

**A redeploy never changes an image by itself.** Bayplan resolves a tag to a digest when a service is new or the
image text you wrote changes (`nginx:1.27` to `nginx:1.28`). A service whose image text is unchanged **keeps the
digest it runs**, however far the registry's tag has moved. An image that already carries `@sha256:` is used as
written. The pin is Bayplan's state, not a change to your file, and it shows as a **digest chip** (click to copy)
on the Overview, in the review and in the Rendered YAML. A standalone host is pinned the same way as a swarm.

- A service that needs a fresh digest needs its registry. If the registry can't be reached, only the services that
  need it are blocked, with the reason. A service that keeps its running digest never needs the registry.
  Registry credentials are the ones stored in Bayplan ([registries.md](registries.md)).
- An image no registry has (built on the host) is pinned to its local image ID, with a warning *isn't from a
  registry*. See [behaviour that is still open](#behaviour-that-is-still-open) for Engine 29.

**Update images** (in the stack's header) is the one way to move a tag. It lists the services that run, with
their digests; you tick what may move, and Bayplan looks each tag up again and opens the same review with the
digest changes. Only the ticked services get a new digest. Services that pin their image in the file, run an
image from no registry or are not deployed can't be ticked. **Bayplan does not tell you that an update is
available**: detection is a later phase, and this is the action you take when you decide to look.

## Deploying

**Deploy stack** starts a job and opens the **console**, which you can leave and come back to (the header's jobs
indicator lists every running stack operation and the pulls). It shows the steps (check the stack, look up
images, networks, secrets and configs, services, remove what is no longer in the stack), a row for each service,
and an output log, all with the stack's secret values removed before anything is stored.

- **On a swarm**, a service is done when it **converges**, not when the swarm accepted the write: the job polls
  each service every 2 seconds until its tasks run on its current spec and its update completed, and the row reads
  *Pulling image*, then *Converging 2/3*, then *Running*. A service that has not settled within the **convergence
  timeout** reads *Timed out* and the deploy stops there: Docker may still finish it. Set the timeout with
  `BAYPLAN_STACK_CONVERGE_TIMEOUT` (default `10m`, from `1m` to `2h`; see the table in the README). Job services
  (`replicated-job`, `global-job`) are not waited for.
- **On a standalone host**, Bayplan prepares the project and runs the Docker Compose engine itself against the
  host, so `depends_on` conditions, health-check waits, recreates when a service's configuration hash changes and
  removal of services the file no longer declares all behave as `docker compose up` does. Each service shows its
  containers going *Pulling image, Creating, Starting, Running, Healthy*. Every image the host lacks is pulled by
  Bayplan **with the stored registry credential** before the engine runs.
- **Your draft is deployed as reviewed.** The deploy checks again, for you, that the draft and the pins are still
  those the review showed, and that the host-access policy still allows them.

You may have at most five jobs running at once.

## When a deploy fails, is cancelled or is interrupted

**Bayplan stops, reports and never rolls back by itself.** The console names the service it stopped at with
Docker's own words (*Docker rejected web: ...*), says how many services were updated before it stopped, and keeps
the failure on the stack as **Deploy failed** until a later deploy succeeds. What was already updated stays
updated.

- **Objects the deploy made that nothing uses are removed**, so a failed deploy does not leave debris: on a swarm
  the new networks and the new hashed secrets and configs; on a standalone host the containers the engine created
  and never started and the networks nothing uses. It never removes a volume, a running container or something
  another stack owns. If a removal fails, the console lists what is left, and the next deploy finds it by label.
- **Cancel deploy** (in the console, with a confirmation) stops at the step the deploy has reached. Services
  already updated stay updated, and the unused objects it made are removed in the same way.
- **Roll back to previous version** redeploys the last version that ran well, as a revision of its own, with its
  stored image digests and **no registry call**, under the same lock and the same host-access check. It is offered
  on a failed or cancelled deploy and on the stack page's failure alert, and it never starts without your click.
  Rolling back across a secret or config change recreates the object that version needs. **Roll back is a swarm
  feature in this version**: on a standalone stack the server answers *this version cannot deploy to a standalone
  Docker host yet*. Fix the file, review and deploy again.
- **A deploy that was running when Bayplan stopped is marked interrupted** when it starts again. The stack reads
  *Deploy failed* with *The deploy was interrupted when Bayplan stopped. The swarm may be partly updated.* The next
  deploy finds what is left by its labels and reconciles it.

## Stop, Start and Remove

All three are jobs with a console, under the [operation lock](#one-operation-at-a-time), and are audited.

**Stop stack…** (a button in the stack's header while it runs) parks a stack without deleting anything it owns.

- On a **swarm**, replicated services are scaled to 0 and their counts remembered on the service
  (`bayplan.stack.stopped-replicas`); global and job services are removed and recreated on Start.
- On a **standalone host**, it stops the containers.
- **Networks, volumes, secrets and configs stay.** The stack reads **Stopped**.

**Start stack** (the header's button while it is stopped, and on the Overview's *is stopped* alert) brings back
**the last deployed version, with the same digests, not your saved draft** (the
button's tooltip says so).

**Remove…** (under the header's **More** menu, beside **Copy stack name**, and in the stack's row menu in the list)
deletes everything the stack owns and needs a typed confirm of the stack's name.

1. The dialog lists what will go: the services (or containers), the stack's own networks, its own secrets and
   configs, and the definition.
2. **Volumes are opt-in.** Each volume the stack owns is listed (per node on a swarm) with a checkbox, unticked.
   Only a ticked volume is deleted, and only if it carries this stack's label on that node. Unticked volumes stay
   on the host.
3. **External networks, secrets, configs and volumes are never touched.**
4. It waits for the tasks and containers to be gone before it removes what they held, so a network is not removed
   under a running container.
5. By default the stack's **definition** (compose file, files, variables and draft) is deleted too. **Keep the
   definition** keeps them and the stack reads *Not deployed*, so you can deploy it again.

If a removal fails part-way, what was removed stays removed and **the definition is kept**: remove again to finish.
A node whose agent is not connected is not waited for, and its volumes are not offered; remove the stack again
once it is back to see them.

## One operation at a time

Deploys, updates, roll backs, stops, starts and removals take a **per-environment operation lock**: one stack
operation runs on an environment at a time. A second is refused with *Another stack operation is running on
{environment}. Try again when it finishes.* and a link to the one that holds the lock, and nothing is stored. The
lock is Bayplan's, so it protects against two people clicking, not against someone using `docker` on the host. The
lock is built to be shared: the janitor of a later phase is designed to take the same one.

## What Bayplan writes on your hosts

Everything a stack creates carries labels, and **Remove, the cleanup after a failure and later phases' orphan rules
find the stack's things by them**, never by a name alone. They are on-host contracts: a network or volume a stack
made keeps its labels for as long as it lives.

| Label | On | Meaning |
|-------|----|---------|
| `com.docker.stack.namespace` | swarm services, networks, secrets and configs | The stack's name, as `docker stack deploy` writes it. A stack that Bayplan did not make but that uses the same name is a foreign stack. |
| `com.docker.compose.project` | standalone containers, networks and volumes | The stack's name, as Docker Compose writes it. |
| `com.docker.compose.service`, `.oneoff`, `.config-hash`, `.project.working_dir`, `.project.config_files` | standalone containers | Docker Compose's own labels. The working directory and config files carry neutral constants (`/bayplan-stack`, `compose.yaml`), never a path of the Bayplan server. A container whose config hash differs from its service is recreated. |
| `bayplan.stack.id` | services, containers, networks, volumes, secrets and configs | The ID of the Bayplan stack record that owns the object. |
| `bayplan.stack.image` | services and containers | The image exactly as you wrote it, before it was pinned. |
| `bayplan.stack.object` | secrets and configs | Marks an object made from one of the stack's files; its value is the name the compose file uses. |
| `bayplan.stack.object.hash` | secrets and configs | The 8-hex-digit content hash that ends the object's name. |
| `bayplan.stack.secret.<name>`, `bayplan.stack.config.<name>` | standalone containers | The content hash of each secret or config the service uses, so new content recreates the container. |
| `bayplan.stack.stopped-replicas` | swarm services | A replicated service's count while the stack is stopped. |

**Hashed object names.** A swarm secret or config made from a stack file is named `<stack>_<name>_<hash>`, cut
(never in the hash) to the 64 characters a swarm allows. The hash is a short content hash, **not a security
control**: 8 hex digits are only enough to tell versions apart, and a low-entropy secret can be guessed from its
hash by someone who can read the object's name and labels (see [threat-model.md](threat-model.md)).

**What Remove deletes**, and only what carries the stack's labels: the services or containers, the stack's own
networks, its own secrets and configs, the volumes you ticked, and the definition (unless you kept it).

## Host access and who may do what

| Who | May |
|-----|-----|
| Viewer (`stacks.read`) | See stacks, their files, reviews, deploys, resources and logs. Secret values stay hidden. |
| Operator (`stacks.update`, `stacks.deploy`) | Save and discard a draft; review; deploy, update images, roll back, stop, start and cancel. |
| Env Admin (`stacks.create`, `stacks.delete`) | Create and remove stacks, and **deploy a stack that uses host access**. |
| Anyone holding `secrets.reveal` | Read a secret variable or a secret file, one at a time, audited. |

A compose file that uses **host access** needs Admin on the environment, and the error names the keys and lines.
The rules run on the **rendered** stack, so `${DATA}:/data` with `DATA=/` is judged as `/:/data`, and run again at
the review, at the deploy (for the person who deploys) and in the job, under the lock. They cover `privileged`,
`cap_add`, `devices`, `device_cgroup_rules`, `pid`, `ipc`, `uts`, `userns_mode`, `cgroup`, `cgroup_parent` and
`network_mode` of the host or of another container, `security_opt` that weakens confinement, a bind mount of the
Docker socket, a system path or any host path outside `/srv`, `/mnt` and `/media` (and strictly below `/var/log`), and a local volume whose
`driver_opts` bind a host path. The converted swarm service is judged as well. A person below Admin who deploys
**a rollback** is judged on the version it restores, so they can't bring back host access an Admin removed.

On a remote host the agent applies its own check to every container create and every service write, and **refuses
host access unless the host owner set `BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`**, whoever asks: see
[agent.md](agent.md). A swarm service that a stack owns is read only on the service pages, with a link **Edit in
stack**.

## The other tabs and the links from other pages

- **Overview**: one state for the stack (*Not deployed, Deploying, Running, Degraded, Not running, Deploy failed,
  Stopped, Stopping, Starting, Rolling back, Removing*, and *Unknown* when Bayplan can't read the engine), the
  services or containers with digest chips, replicas and ports, the last deploy and who made it, and banners for a
  running operation, a saved draft that is not deployed and a stopped stack. A failed last deploy is never hidden
  behind *Running* or *Degraded*.
- **Resources**: everything the stack owns on the environment, found by its labels: the tasks or containers, the
  networks, the volumes (per node, naming nodes that did not answer), and the secrets and configs with their
  version and the services that use them. An object the file only refers to is marked **External**.
- **Logs**: the merged log of every service of a swarm stack, or every container of a standalone one, with
  coloured prefixes and replacement markers, narrowing by service, task, node or container, a download, and a
  pop-out. **Bayplan hides the secret values it knows** (the stack's secret variables and secret files, 4
  characters or longer) in every streamed and downloaded line; a secret an application makes up itself is shown as
  written. A log that can't be masked is not served.
- **Links.** A stack's name, wherever it appears (the services, containers, secrets and configs lists, their
  detail pages' owner and the managed-by-stack notices), links to the Bayplan stack that owns it. A stack Bayplan
  does not manage stays plain text.

## Limits

| What | Limit |
|------|-------|
| Stack name | `^[a-z0-9][a-z0-9_-]{0,62}$`, fixed for life |
| Files | 50 per stack, 1 MiB each, 8 MiB in all; a path of at most 255 characters, a name in it at most 100 |
| Secret file | 500 KB on a swarm, 1,000 KB on a standalone host (config: 1 MB, 1,000 KB) |
| Secret variable | at least 4 characters |
| Convergence | each service `BAYPLAN_STACK_CONVERGE_TIMEOUT`, 10 minutes by default, 1 minute to 2 hours |
| Jobs | 5 running per person; one stack operation per environment |
| Checks | the editor's check has a 5-second deadline, and 2 run at once per person |
| Rendered YAML diff | bounded (20,000 lines, 4,000 edits) |
| Console output | the newest 2,000 lines |
| Logs | a stack's log follows up to 32 streams (a swarm's services, a host's containers), 8 log streams per person |
| Lists | at most 500 stacks come back in one list, after search and filters |

## What is recorded

Every stack action writes an audit row, with the stack and who did it and never a value: `stack.created`,
`stack.draft_saved`, `stack.draft_discarded`, `stack.deployed` (with its mode: deploy, update or rollback; its
result, revision, job and service count), `stack.stopped`, `stack.started`, `stack.removed` (how many volumes were
removed and kept, and whether the definition was kept), and `stack.value_revealed` (the kind and name of the
variable or file shown, never its value). Opening and downloading a stack's logs write `logs.viewed` and
`logs.downloaded` with the stack as the target and none of the lines. See [access.md](access.md) for the audit log itself.

## Behaviour that is still open

These are how Bayplan behaves today. Some are decisions the maintainer has not made, not oversights, and none of
them is hidden.

- **`sysctls` and `ulimits` are not judged as host access.** Anyone with `stacks.deploy` (an Operator) can set
  them in a stack, and an Operator can already set them on a service.
- **On Docker Engine 29, an image that exists only on the host fails with `image_unresolved`.** The containerd
  image store lists a `name@sha256:...` for every image, including one never pushed, so Bayplan can't tell a local
  image from one a registry lost, and a deploy of it stops at *Look up images*. Engines 24 and 25 pin it to its
  image ID with a warning. Push the image to a registry, or write its digest in the compose file.
- **A standalone service with `pull_policy: always`, `daily` or `weekly`** makes the Compose engine pull again,
  anonymously, after Bayplan pulled with your credential. A pinned digest means it can't move to a newer image, but
  a private image fails at that pull.
- **Behind an agent, `network_mode`, `pid` or `ipc` of `service:X`, and `volumes_from`, are refused as host access**
  unless the host owner set `BAYPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`. On the Bayplan server's own host they
  work. The review does not warn about it.
- **An image update deploys the current draft.** **Update images** opens the same review and the same deploy, so
  edits in a saved draft that were never deployed go out together with the new digests. The review lists them
  all, and only the ticked services get a new digest.
- **A secret or config written `environment: VAR` cannot be deployed while `VAR` is in the stack's variables
  table.** It is a known bug (the object resolver counts two sources) with a fix proposed. Use a stack file under
  `file:` instead.
- **Roll back is not available on a standalone stack** (see [above](#when-a-deploy-fails-is-cancelled-or-is-interrupted)).
- **A network or a volume a file stops declaring is not removed** on either target, as `docker compose up` does
  not either; Remove deletes the networks the stack owns, and the volumes you tick.
- **A secret or config that is declared and used by no service is not created.** `docker stack deploy` creates
  every declared secret and config; Bayplan creates the ones a service uses.
- **A changed secret or config on a standalone host reads "changed, 0 settings"** in the review, because its
  content hash is a Bayplan label the comparison leaves out, and the Other resources table lists no secret or
  config rows there.
- **A secret file of the largest size made of one repeated character can take minutes to check and review**, and
  one of about a megabyte of ordinary content takes seconds.
- **The Remove dialog's progress panel disappears** when the stack's own page turns into *This stack is gone*; the
  answer arrives as a toast.

## What Phase 7 does not do

These are decided and planned for later phases. None of them is in this version.

- **A revision list, and roll back to any revision.** Bayplan keeps the last deployed version for **Roll back to
  previous version**, not a history you can browse.
- **Adopting a stack Bayplan did not create**, discovering foreign stacks and showing **drift** between a stack and
  what runs. A foreign stack is only refused when its name is taken, never taken over.
- **Git-backed stacks.** Every stack is *Inline*: its files live in Bayplan.
- **Update detection.** Nothing says an image has a newer version, and there is no fleet view of updates.
- **A janitor** for old objects (it will share the operation lock).
- **Variables shared across stacks or environments.**
- **A folder on the host, synced by the agent, for relative bind mounts.**
- **Delivering a secret variable as a Docker secret.**
- **Building images**, and `include` and `extends`.
