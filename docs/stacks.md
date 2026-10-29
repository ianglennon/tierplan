<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Bayplan Authors -->

# Compose stacks

A stack is a compose file that Bayplan deploys and keeps tidy: its services, networks, secrets and configs go
together, and nothing is left behind when you remove it. This page explains how to write one, what the review
shows before anything changes, what a deploy does on a swarm and on a standalone host, how it stops, starts and
goes away again, how to see its history and roll back, how to take over a stack that something else made (for
instance Portainer), how Bayplan spots changes made behind its back, how a stack can follow a folder of a git repository, and what
Bayplan writes on your hosts. It
also says plainly what is not in this version, and a few behaviours that are open decisions, not oversights.

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
- [History and roll back](#history-and-roll-back)
- [Unmanaged stacks](#unmanaged-stacks)
- [Adopting a stack](#adopting-a-stack)
- [Drift](#drift)
- [Git stacks](#git-stacks)
- [One operation at a time](#one-operation-at-a-time)
- [What Bayplan writes on your hosts](#what-bayplan-writes-on-your-hosts)
- [Host access and who may do what](#host-access-and-who-may-do-what)
- [The other tabs and the links from other pages](#the-other-tabs-and-the-links-from-other-pages)
- [Limits](#limits)
- [What is recorded](#what-is-recorded)
- [Behaviour that is still open](#behaviour-that-is-still-open)
- [What is not in this version](#what-is-not-in-this-version)

## Create a stack

**Stacks** is the first item in each environment's sidebar, and **All stacks** lists the stacks of every
environment you may read, grouped by environment, with search, status chips and sorting. Creating a stack needs
the Admin role on the environment (`stacks.create`).

**Create stack** asks for a name and a starting point (the starter example, an empty file, or a compose file you
upload). The name starts with a letter or number and **can't change later**: it is the swarm namespace or the
Compose project name on the host. The rule for it is under [Stack names](#stack-names). Bayplan checks it as you
type, against the environment you create the stack on.

A stack whose name a stack on that environment already has, that Bayplan does not manage, is refused: *a stack
named X already exists on this environment, not managed by Bayplan*, with the advice to pick another name or
[adopt the existing stack](#adopting-a-stack). Bayplan never silently takes over a stack. The same check runs
again at the review and at the deploy.

Creating a stack sends nothing to any engine. It reads **Not deployed** until you deploy it.

### Stack names

One rule decides a stack's name, whether you create the stack in Bayplan or adopt one that something else made.
There is no second, stricter rule for creating.

| Target | A name is | Longest |
|--------|-----------|---------|
| A swarm | letters in either case, numbers, dash and underscore; starting with a letter or number; no dot | 61 characters |
| A standalone host | Compose's project-name rule: lowercase letters, numbers, dash and underscore; starting with a letter or number; no dot | 255 characters |

Why a swarm is stricter: it names a stack's services and networks `{stack}_{key}`, and it takes only letters,
numbers, dash and underscore in such a name, up to 63 characters. A dot would make a service that can never be
created, and 61 is the longest stack name that still leaves room for a one-character service. So **a stack name
with a dot, a space or a slash is refused** (*Use only letters, numbers, dash and underscore, ...*), and an
unmanaged stack with such a name is listed but cannot be [adopted](#adopting-a-stack), with the same reason. A
swarm stack may have capital letters now (`MyApp`); Phase 7 allowed only lowercase.

Two more consequences on a swarm:

- **A name swarm would refuse is stopped at the check, not half way through a deploy.** The name of a service or a
  network the stack would create is `{stack}_{key}`, and when the key is long or has a dot, or a network has an
  explicit `name:` swarm refuses, the check reports an error `swarm_name_invalid` on that key and the deploy does not
  start. A stack with a 61-character name and no network of its own is such a case: its implicit network
  `{stack}_default` is 8 characters longer than the name, so give it a short explicit network name or a shorter
  stack name.
- **Two stack names that differ only in case are the same name.** Swarm treats `myapp` and `MyApp` as one, so
  creating the second is refused as taken (*409 `stack_name_taken`*).

## The editor

A stack page has five tabs: **Overview**, **Editor**, **History**, **Resources** and **Logs**. The Editor has three panes:

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
ordinary words. Bayplan hides the value as stored and in the escaped forms error messages write it in, and a
value of several lines, such as a PEM key, is also hidden line by line (each line of 4 characters or more, for the
first 200 lines of a value up to 64 KB; a longer value is hidden whole). When a value that comes from a secret variable can't be used (for example `mem_limit: ${PW}`), the check, the review, the deploy, the roll back and the start name the field and say that the value comes from a secret variable. They never show what the compose loader or the swarm converter said about it, because that can contain part of the value. The server log gets the same sentence for a conversion a job could not make. A message about a value that is not secret keeps the loader's words. Bayplan decides this from where the value comes from in the file, never from what the message says. When a message names no field, or a field Bayplan can't find in the file, and the file uses a secret variable, it says only that a value isn't valid and that the file uses a secret variable. An item of a list (a volume, a device rate) is judged by its whole list, across every YAML document of the file, because compose merges the documents and repeated items, so a position in a list is not one line of the file. When any item of that list uses a secret variable, a message about any of its items says only that a value isn't valid and that the file uses a secret variable. Secret variables are interpolated like any other: they are **not** turned into Docker secrets.
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
- **Other resources**: networks, volumes, secrets and configs added, changed, removed or external. On a standalone
  host it also flags a network or volume whose settings on the host differ from the compose file (see
  [below](#network-and-volume-changes-in-a-standalone-review));
- **Rendered YAML**: a text diff of the rendered stack **against the last deploy**.

The field diff and the Rendered YAML answer different questions, and both are labelled. **The field diff compares
with what is running on both targets**: on a swarm it reads each service's current spec, and on a standalone host
it rebuilds every running container's settings. A container changed by hand on the host (`docker update`, for
instance) is therefore listed as changed at that setting even though your compose file did not change, and the
next deploy recreates it. **The Rendered YAML tab compares with the last deploy Bayplan made**, so it shows what
you changed in the file, whatever someone did on the host.

Two more parts appear only when they apply, and neither holds a deploy back. **Reverts drift** lists the changes
somebody made outside Bayplan that this deploy would put back (see [Drift](#drift)). **Ownership labels only** groups
the services or containers of a freshly [adopted stack](#adopting-a-stack) that the deploy recreates only to take
Bayplan's labels.

**Errors stop the deploy** and are listed with a link to the line; **Deploy stack** says why it is off.
**Warnings do not.** A review of a stack that is the same as what runs says *No changes* and has nothing to deploy.

**A concurrent-deploy warning.** A draft remembers the deployed version it started from. If someone deployed
since, the review shows *This stack was deployed after you started editing* and a **See what changed** view
(what the draft started from, what is deployed now, your changes, secrets hidden), and **Deploy stack** stays off
until you tick that you have seen it. The server enforces this as well as the page. Bayplan never overwrites
someone's deploy silently.

A review right after a successful deploy is empty on both targets.

### Network and volume changes in a standalone review

**On a standalone host, the review also compares each network and volume the stack declares with the one that
exists on the host.** When the object's configuration hash on the host (the `com.docker.compose.config-hash` label
Docker Compose wrote when it made it) differs from the one the compose file gives, the review adds a row to **Other
resources**:

- a **network** row: *Its settings differ from the compose file. The deploy recreates it, which restarts the
  containers attached to it.* Docker Compose recreates a network whose settings changed, and no data is lost, but
  the containers on it restart;
- a **volume** row: *Its settings differ from the compose file. Bayplan keeps it as it is.* A volume is never
  recreated (see [Volumes are never recreated](#volumes-are-never-recreated)), so the change is not applied.

**This applies to every standalone stack, those created before this release included.** A standalone review in
Phase 7 had no such rows. Objects the file marks `external`, objects that do not exist on the host yet and objects
with no hash label are not flagged.

## Image pinning and Update images

**A redeploy never changes an image by itself.** Bayplan resolves a tag to a digest when a service is new or the
image text you wrote changes (`nginx:1.27` to `nginx:1.28`). A service whose image text is unchanged **keeps the
digest it runs**, however far the registry's tag has moved. An image that already carries `@sha256:` is used as
written. The pin is Bayplan's state, not a change to your file, and it shows as a **digest chip** (click to copy)
on the Overview, in the review and in the Rendered YAML. A standalone host is pinned the same way as a swarm.

- A service that needs a fresh digest needs its registry. If the registry can't be reached, only the services that
  need it are blocked, with the reason. A service that keeps its running digest never needs the registry.
  Registry credentials are the ones stored in Bayplan ([registries.md](registries.md)).
- **On a swarm, when Bayplan looks an image up it also writes the image's platforms** (for example `linux/amd64` and
  `linux/arm64`) into the service's placement, as `docker stack deploy` does with its default
  `--resolve-image=always`. The swarm then schedules the service's tasks only on nodes that can run the image. A kept
  digest keeps the platforms recorded for it, and asks no registry for them. A service deployed before this version
  has none recorded: it gains them the next time its image is looked up (**Update images**, or a changed tag),
  because a kept digest never asks a registry.
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
  timeout** reads *Timed out* and the deploy stops there: Docker may still finish it. Two causes do not wait for
  the timeout:
  - A service the swarm will not start again reads *Failed* within a few seconds, with the task's own error
    (*task: non-zero exit (1)*), and the deploy stops there. Its tasks stopped and its restart policy is used up:
    `max_attempts` was reached, `condition` is `none`, or the task exited 0 under `on-failure`. Check the
    service's restart policy and its logs, or roll back.
  - A service whose tasks are waiting because a network of the stack
    has no free addresses reads *Failed*, and the console names the network and its subnet. Give the network
    a larger subnet under `networks.<name>.ipam` (a network with no `ipam` gets a /24, 253 addresses, shared by
    every task, service virtual IP and node of the stack), or run fewer tasks, then deploy again. Bayplan reads
    this from a manager, which needs Docker Engine 29 or later. On an older engine such a service reads *Timed
    out*, and the console says how many tasks were still waiting for an address.

  Set the timeout with
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

### Volumes are never recreated

On a standalone host, the Docker Compose engine notices when a named volume exists but its driver, options or
labels differ from the compose file, and it asks: *"Recreate (data will be lost)?"*. Answering yes removes the
volume and creates it again, which deletes the data in it. **Bayplan always answers no** (decision D-46): a deploy
whose compose file changes a volume's driver, options or labels leaves the volume as it is, goes on, and the
containers mount the existing volume with its data. The change is not applied, and the console log says which
volume was left alone.

To change those settings, remove the volume by hand on the **Volumes** page and deploy the stack again. Docker
refuses to remove a volume a container still mounts, so the containers that use it must be gone first. The volume
is then created with the new settings and is empty, so copy the data out first if you need it.

This changed in Phase 8: a standalone stack deployed with an earlier version recreated a volume whose settings
differed, so the same edit that was applied then is left alone now. Networks are different: the Compose engine
still recreates a network when its settings change, which restarts the containers attached to it and loses no data.

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
  Rolling back across a secret or config change recreates the object that version needs. It works on a swarm and on
  a standalone host, and it is the one-click form of [rolling back to a chosen revision](#history-and-roll-back).
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
   On a standalone host the containers include any that someone added to one of the stack's services from the host's
   CLI (for example with `docker compose up --scale`), and Remove waits for them like the rest, so nothing of the
   stack is left.
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

## History and roll back

Every deploy is stored as a **revision** of the stack: its compose file, its other files, its variables, the image
digests it pinned and the compose file as it was rendered. The **History** tab, after Editor, lists them newest
first. History only grows: nothing is rewritten, and a roll back adds a revision of its own.

| Column | Shows |
|--------|-------|
| Revision | Its number, and *Deployed* on the newest good one, which is what runs now |
| Kind | *Deploy*, *Update images*, *Roll back* (with the revision it restored, *of #5*) or *Adopted* (*from* the namespace or project it was taken from) |
| Result | *Good*, *Failed* (with the service and the reason, in the words of the failed-deploy alert) or *In progress* |
| Who, When | Who started it and when |
| Images | The digest changes against the previous good revision, or the number of images pinned for a first or adopted revision |

Select a revision to open its card. It has four tabs:

- **Changes** (the first): per service, what rolling back to this revision would change against **what runs now**,
  field by field. The values of environment variables, commands, health check commands and log options are
  withheld from anyone without `secrets.reveal` (the path that changed is still listed). A setting that differs
  because the stack [drifted](#drift) carries a *Drifted* mark.
- **YAML diff**: the rendered YAML of this revision against the deployed revision.
- **Rendered**: the revision's rendered compose file as it was stored, read only, with Copy and Download.
- **Contents**: its files (a secret file reads *Hidden*), its variables (a secret one is hidden) and its pinned
  images. A roll back restores all of this exactly.

A secret variable and the content of a secret file never leave the server from a revision. The other values in a
rendering, such as plain variables, are shown to anyone who holds `stacks.read`, as the draft is.

There is no comparison of two arbitrary revisions, and no *open this revision as a draft* action: roll back, then
edit.

### Rolling back to a revision

An Operator (`stacks.deploy`) can roll a stack back to **any revision that finished well**, on a swarm and on a
standalone host. Press **Roll back to revision #n** on the revision's card, or in the row's menu. A *Failed* or *In
progress* revision has no such action: it stays in the list for the record. The page hides the action on the deployed
revision, since there is nothing to restore; the server would accept it, and it is how [restoring from
drift](#drift) works.

- **It goes through the review.** The review compares the revision with what runs now and lists the Images as
  *Pinned (revision #n)*. Nothing else changes until you press **Roll back stack**.
- **It restores everything exactly**: the compose file, the other files, the variables and the pinned image
  digests. **It never looks anything up in a registry**, so a tag that has moved since cannot change what comes
  back. A secret or config that version used is recreated from its stored content.
- **The platforms come back too.** A revision stores the platforms its images were deployed with, next to their
  digests, and a roll back restores them without asking a registry.
- **Your draft stays as it is.** After the roll back the draft shows the notice that the deployed version changed
  since you started editing, and its review asks you to acknowledge it, as for any deploy by someone else.
- **It is recorded as a new revision** (for example #9, *Roll back of #5*), audited as `stack.deployed` with mode
  `rollback` and the revision it restored.
- The same rules as a deploy hold: the [operation lock](#one-operation-at-a-time), and the host-access check, which
  judges the version being restored for the person who asks (so someone below Admin cannot bring back host access an
  Admin removed). If the stack was deployed again since you opened the review, the roll back is refused (*The stack
  was deployed again since you opened this review*) and you review again.

### How many revisions are kept

Bayplan keeps the last **N** revisions of each stack. N is **Revisions kept per stack** under **Settings, General,
Stack history**: 20 by default, from 5 to 200, one value for the whole instance, changed by an instance admin
(`settings.write`).

- Never deleted, however old: the **newest good revision (the deployed one)** and any revision whose deploy is still
  running. The draft is not a revision and is never touched.
- A deleted revision takes its files and variables with it, **sealed secret variables and secret files included**,
  so old secrets do not live for ever. Its card then reads *That revision is gone*.
- Pruning runs after every deploy's outcome is recorded, at start-up, and when you lower the limit. Raising the limit
  deletes nothing.
- **Lowering the limit asks first.** The dialog says how many revisions across how many stacks go (*Save and delete
  n revisions*), and they cannot be recovered. The change is audited as `settings.updated` with the old limit, the
  new one and the number deleted, in the same transaction as the deletion.
- Deleted rows leave free pages in the database file that are not wiped. What was sealed there is encrypted under the
  master key (see [threat-model.md](threat-model.md)).

## Unmanaged stacks

Bayplan lists the stacks it did not create (made by `docker stack deploy`, `docker compose up` or Portainer) beside
its own, read only. They are **Unmanaged**. Nothing about them is stored: Bayplan reads them from the engine each
time, and Docker stays the source of truth.

**What counts as a stack.** Only labels define one. On a swarm, the services that share a
`com.docker.stack.namespace` label are a stack. On a standalone host, the containers that share a
`com.docker.compose.project` label are a stack (a one-off container from `docker compose run` is left out). A
service or container with neither label is not a stack and stays on the Services or Containers page.

**Where they show.** The environment's stack list and **All stacks** list them with an *Unmanaged* badge, a count in
the caption (*3 stacks, 1 unmanaged*) and a **Show** filter with *All*, *Managed* and *Unmanaged* (kept in the page
address, so a link keeps it). Their row menu offers *Open stack* and *Adopt stack…*. If Bayplan cannot read the
engine to look for them, the list says *Couldn't look for unmanaged stacks on {environment}* with a **Retry**, and your
own stacks still list. A stack that Bayplan is deploying for the first time is never listed twice.

**The page of an unmanaged stack** has **Overview**, **Generated compose**, **Resources** and **Logs**, and no
Editor or History: Bayplan holds no definition of it. Its note says so: *You can look at it here. Adopt it to deploy,
update and roll it back with Bayplan. Adopting changes nothing until you deploy.*

- **Generated compose** is a preview of the compose file [adoption](#adopting-a-stack) would use, rebuilt from the
  running services. Nothing is saved. It needs only `stacks.read`, and **for anyone without `secrets.reveal` every
  environment value, command, entrypoint, health check command and log option is `<hidden>`**, as are those fields
  in the adopt check. The names of the secrets and configs it references are shown and their values are never read.
- **Resources** shows what the stack's services use, and which networks and volumes would be owned by the stack
  after adopting (those carrying its label) and which stay external.
- **Logs** is the merged log of the stack, with the same streams, limits and audit as a managed stack's.
- The only action is **Adopt stack** (Admin). There is **no Remove** on an unmanaged stack: adopt it first, or remove
  its objects one by one on their own pages (*To remove it, adopt it first.*). The normal actions on its service and
  container pages are unchanged.

**Flags on an unmanaged stack:**

| Flag | Meaning |
|------|---------|
| *Portainer* | Looks like Portainer made it: a label whose key starts with `io.portainer.`, or a Compose working directory under `/data/compose/`. The page says: *Remove it from Portainer without deleting it, or the two tools will fight over it. Bayplan doesn't contact Portainer.* See [behaviour that is still open](#behaviour-that-is-still-open) for how far those markers are verified. |
| *Created by Bayplan (another instance or a lost record)* | Its objects carry a `bayplan.stack.id` that no stack of this instance has, for instance after a restore from an old backup, or because another Bayplan made it. The label is only a note: it never links the stack to anything. It is adopted like any other, and the first deploy replaces the old ID. |
| *Bayplan*, *Bayplan agent* | The stack Bayplan itself runs in, or one that runs a Bayplan agent. It is listed, and **Adopt is off** (*This is Bayplan's own stack.*, *This stack runs Bayplan's agent.*), so Bayplan cannot be made to deploy or remove itself. |
| *Can't be adopted* | Its name breaks the [stack-name rule](#stack-names), for example a space or a slash. The reason is shown. |

## Adopting a stack

**Adopting** turns an unmanaged stack into one Bayplan manages fully, so you can edit, deploy, update, roll back,
stop and remove it, and see its drift. It is how you move off Portainer. It needs the Admin role on the environment
(`stacks.create`; adoption adds no permission of its own). The check beforehand needs only `stacks.read`.

**Adopting changes nothing on the engine.** Pressing **Adopt stack** stores a record in Bayplan, and no request is
sent to Docker. The first deploy afterwards is what changes things, and it is **not restart-free**: see [the first
deploy](#the-first-deploy-of-an-adopted-stack).

Open the stack's page and press **Adopt stack** (or use the row menu). Work through the adopt page in three steps.

### 1. Choose where the definition comes from

- **Rebuild from the running services** (the default). Bayplan reads what runs and writes a compose file from it. It
  is the reverse of what a deploy does, and it leaves out engine defaults so the file reads like one a person would
  write. A stack that cannot be written as a compose file that passes the check is refused with the reason
  (`rebuild_failed`).
- **I have the original compose file.** Paste it (or choose a file, up to 1 MiB), fill in the values of any variables
  it uses, and Bayplan checks it against what runs. It says *Matches what's running* or *Differs from what's running
  in n settings*, with the differences, and **Use the rebuild instead** is always one click away. A file that
  matches is used **exactly as you wrote it** (comments and key order kept). The file goes through the same check as
  any draft, so `include`, `extends` and files on the Bayplan server are refused, and the server checks it again when
  you press Adopt. The variable values you type are saved with the stack (a secret one sealed). Nothing of the file
  or the values is kept in the browser after you leave the page.

### 2. Read the check

Before anything is stored, the page runs a review of the definition against what runs, with these results.

- **What happens on the first deploy.** *Nothing will change except Bayplan's ownership labels*, or a list of
  **differences** (*3 differences will change on the first deploy*) with what runs now and what the first deploy
  would set. **Differences are shown, and they do not block adopting.** A rebuild is close but not always perfect, so
  read them: for example a setting the engine reports in a form the compose file cannot say. The ownership labels are
  counted apart: *2 services will be recreated to take Bayplan ownership labels (brief restart)*.
- **How the pieces are treated.** A table of every network, volume, secret and config:
  - a **network or volume that carries the stack's label** (`com.docker.stack.namespace` or
    `com.docker.compose.project`) is **owned by the stack under its existing name** and is not recreated. A later
    Remove cleans it up (a volume only if you tick it);
  - a network or volume **without** the label is **External**. Bayplan does not change it;
  - an existing **swarm secret or config** is referenced as **external** and left untouched: *Not stack-owned. Move it
    to a stack file to rotate it.* Bayplan never reads their values and does not import their contents;
  - on a standalone host a network or volume is written in the form whose configuration hash equals the one on the
    host, so the Compose engine does not see a change. If no form matches, the object is kept **external** with a
    note, so the first deploy cannot recreate it.
- **Host access.** If the definition uses settings that need Admin (a privileged container, the Docker socket, a host
  path), the page lists them (*Deploying this stack needs Admin*). Adopting is fine; the policy is judged when you
  deploy, as for any stack.
- **Portainer** and **orphan ID** advice, when they apply (see [Unmanaged stacks](#unmanaged-stacks)).
- **Environment values are copied in as literals.** The definition holds each service's environment values as they
  are, **passwords included**, in the stored draft and in revision #1. Bayplan does not guess which ones are secret.
  After adopting, move secret values to **Variables** as secret variables and refer to them as `${NAME}`. Until you
  do, anyone who holds `stacks.read` on the environment can read them in the draft and in History's *Rendered* and
  *YAML diff* views of revision #1. [threat-model.md](threat-model.md) records this as an accepted residual risk.

### 3. Adopt

Press **Adopt stack**. The page sends the fingerprint of the state it checked. The server reads what runs again,
rebuilds the definition itself (the browser sends no definition, only your pasted file if you chose that) and
compares: if what runs changed since the check, it refuses (*What is running changed since the check. Run the check
again before you adopt.*, with **Check again**) and stores nothing. It takes the environment's [operation
lock](#one-operation-at-a-time) for the moment of storing, and it is refused while another operation holds it.

In one step it then stores:

- the **stack**, with *Adopted* as its source and the namespace or project as its reference. It reads as running or
  degraded straight away and shows no draft badge;
- the **draft** and **revision #1** (*Adopted from {name}*, Good), both holding the definition, with the images
  **pinned to the digests that run**, so no tag is looked up and nothing can move;
- a record of what it adopted: the networks and volumes it owns, the secrets and configs it references, the old
  Bayplan ID if there was one, and the places where the definition differed from what runs.

The audit log gets `stack.adopted` (the stack, its target, where it was taken from, the number of services and the
revision).

A stack that is Bayplan's own or an agent's, or whose name breaks the rule, is refused (`stack_not_adoptable`), and a
name Bayplan already manages is refused with a link to that stack (`stack_already_managed`).

**Names.** Adopting takes any name the [stack-name rule](#stack-names) allows, which includes capital letters on a
swarm that Phase 7 refused. Docker itself accepts more than that rule (a dot, for example), but a name outside it
is listed and cannot be adopted, because a stack named so could not be deployed again.

### The first deploy of an adopted stack

An adopted stack's Overview says *{name} is adopted but not yet deployed by Bayplan* until its first deploy. The review
of that deploy shows **Ownership labels only** apart from real changes, with how many services or containers are
recreated.

- **The first deploy puts Bayplan's labels on everything**, as for any stack, so removal, history and later phases
  find the stack's things the same way. A label change **restarts the workload briefly**: on a swarm Docker restarts
  the tasks of each service whose container labels change, and on a standalone host Docker Compose recreates each container, because the labels
  change its configuration hash. Plan the deploy for a time a restart is acceptable. The review says so plainly before
  you press anything.
- **Platforms are kept.** A stack made with `docker stack deploy` carries the platforms the CLI wrote into each
  service's placement. Adopting records them with the first revision, and the first deploy sends them again, so the
  review shows no placement change.
- **A named volume's mount gets the label too.** Bayplan's `bayplan.stack.id` is added to the volume options of a
  service's mount of a named volume. It counts as an ownership label, so the first review lists it under *Ownership
  labels only*. The volume itself is not touched.
- **Data is kept.** The networks and volumes the stack owns are reused under their names. A volume is never
  recreated, and the first deploy of an adopted standalone project was proved on real engines to keep a marker file
  in a volume. Secrets and configs that already exist are not created, changed or removed.
- **The differences from the check are applied** by this deploy, since the definition is what gets deployed.
- **Host access is judged here**: whoever deploys needs Admin if the definition uses it.
- After a first deploy that went well, Bayplan treats a service or container of the same namespace or project that
  has no Bayplan ID as someone else's, and a deploy that would clash with it stops (*a stack named X already exists
  ... not managed by Bayplan*). Until then such workloads count as part of the stack.

**Differences found at adoption are shown as drift until then.** If the definition differs from what runs, the
stack's [drift](#drift) page lists those differences with the cause *Differed from the rebuilt definition when {name}
was adopted*, and they are cleared by the first deploy.

### Removing an adopted stack

Remove works as for any stack: the services or containers, the networks the stack owns, the definition, and the
volumes you tick. **It leaves the secrets and configs the stack only referenced**, because Bayplan never owned them.
The Remove dialog lists them as external. Delete them on the **Secrets** and **Configs** pages when nothing else uses
them. An external network is left too, as for every stack.

### What adopting owns

Later phases decide what is safe to clean up (Phase 10, orphaned networks and volumes; Phase 11, the janitor). They
must not infer an adopted stack's ownership from `bayplan.stack.id` labels alone.

- **Networks and volumes are owned by name.** The stack's record lists the engine names of the networks and volumes
  that carried its label when it was adopted. They are the stack's **for as long as the stack exists**, and they are
  **never recreated, so they never get a `bayplan.stack.id` label**. A rule that finds a stack's objects by that label
  would call them orphans.
- **Secrets and configs it references are not owned.** They are recorded by name as external and are never removed
  with the stack.
- **A `bayplan.stack.id` label alone never links an object to a stack.** Another ID is another stack's. Between the
  adoption and its first good deploy, a service or container of the stack's namespace or project counts as its own if
  it has no ID or the old one recorded at adoption; afterwards only its own ID counts.
- **On a standalone host, a container added to the stack's project from the host's CLI counts with the stack.** A
  container of the project with no `bayplan.stack.id` is the stack's once the stack owns another container of the
  same service (drift, the stack's page, restore and Remove). Another stack's ID never counts, and a project the stack
  holds no container of, or a service it runs no container of, stays a name clash and is not taken over.
- The one place that decides this is `Ownership` in `internal/stackops` (`OwnershipOf(stack)`). A guard test fails on
  code elsewhere that decides ownership by a Bayplan ID label. Ask it, do not copy the rule.

## Drift

**Drift** is a difference between what a managed stack's **deployed revision** says and what runs. Bayplan checks for
it so a hot-fix made on a host, or a service scaled by hand, does not stay unnoticed until the next deploy quietly
puts it back. **Bayplan shows drift and never fixes it by itself**, and there is no way to dismiss it: you restore
the revision, or you keep what runs.

**What counts.** On a swarm, each service of the stack is compared with the deployed revision (converted with the
digests it stored). On a standalone host, each container of the stack's project is compared by its configuration hash
and then field by field, and its image against the revision's pin. Any difference in a setting counts: replicas,
image or digest, environment, ports, labels, mounts, placement (including the platforms the image runs on), restart
policy and the rest. A revision stored before platforms were recorded is compared through the platforms of the same
image, so an old revision does not read as drift. A service the revision
has and the engine does not is **Missing**; a service or container the stack owns that the revision does not declare
is **Extra**.

**A container added from the host's CLI counts with the stack.** On a standalone host, a container someone adds to the
stack's Compose project from the host, such as `docker compose up -d --scale web=2 --no-recreate` run on your own copy
of the file, carries none of Bayplan's labels, but it belongs to the stack's project. Bayplan counts it with the
stack while the stack owns another container of the same service: a second container of `web` reads as
**web Changed** with two containers, and a restore removes it. The same container shows on the stack's page, in its **Resources** tab and in its logs, does not
block a deploy, a restore, Stop or Start as a name clash, and is listed and removed by **Remove**. A container that
carries **another Bayplan stack's ID** never counts as this stack's. A Compose project of the same name that the stack
owns **no container of** is not taken over: it stays a name clash, so Bayplan never acts on a project it did not deploy
or adopt. Nor is a container with none of Bayplan's labels of a service the stack runs **no container of**, such as a
`postgres` from an unrelated `compose.yaml` whose project happens to have the same name: it is not drift, and a deploy,
restore or Remove is refused as a name clash until you remove it or rename one of the projects.

**What is not drift:**

- Bayplan's own ownership labels (including the one on a named volume's mount), and the swarm's force-update counter. A **restart** or a **force update** does
  not change the spec and is not drift.
- Runtime state such as task churn and a service's update status.
- A **stopped** stack. Stop scales it down on purpose, and it is not checked until it is started.
- A stack that was never deployed.
- Networks, volumes, secrets and configs are not compared on their own; only the services and containers are.

If the deployed revision cannot be redeployed any more (for instance a file it needs is gone), the page says it
*couldn't compare* instead of guessing.

**Scaling from Bayplan's own service page counts as drift**, because the next stack deploy would put the old count
back.

### Where you see it

- The stack lists show a **Drifted** badge after the name, and a **Drifted** status chip (with the number of stacks) to
  filter by. A stack nobody has checked yet, a stopped one and a never-deployed one show nothing.
- The stack's **Overview** shows an alert (*{name} has drifted from revision #n*, with **View drift**) and a *Drift* row
  in the Details card (*3 differences*, a link).
- The **drift page** (not a tab; open it from the alert or the Details card) has a summary of how many services or containers changed,
  went missing or are extra, and for each a *Deployed* against *Running now* table. Values of environment
  variables, commands, health check commands and log options are withheld from anyone without `secrets.reveal`.
  **Check again** reads the engine at once.

### When Bayplan checks

The check is read only and runs in the background for every environment, so the badge is there before anyone opens the
stack. It runs

- about **every five minutes**, with a little random spread so environments do not all read at the same moment;
- **a few seconds after a Docker event that names the stack** (a 3-second quiet period lets a burst of events become
  one check). On a swarm the event carries only the **service's name**, not its stack, so Bayplan finds the stack
  that owns a service of that name. Drift is therefore found within seconds, and the five-minute timer is the backstop;
- when an operation on the stack (deploy, update, roll back, stop, start) ends;
- when you open the Overview or the drift page.

It never runs **while a stack operation holds the environment's [lock](#one-operation-at-a-time)** (a deploy under
way is not drift), and it never writes to an engine. A remote environment is checked while at least one of its agents
is connected.

### Why it differs

Under each drifted service the page says what Bayplan knows from its **audit log**, and nothing it cannot back up:

- *web was scaled from 1 to 3 by alice in Bayplan, 5 minutes ago.* (a scale, a roll back or an edit made from the
  service pages);
- *Changed outside Bayplan.* when no audit row explains it;
- *Differed from the rebuilt definition when the stack was adopted* until the first deploy, for a difference found at
  [adoption](#adopting-a-stack).

The badge's tooltip says the same, shortened. A scale only explains a change of the replica count, never an image
change made after it.

### What you can do

- **Redeploy to restore** (Operator, `stacks.deploy`). On the drift page and the Overview alert. It opens a review of
  the **deployed revision against what runs**, so its changes are exactly the drift, with the revision's stored
  digests and no registry lookup. **Restore stack** deploys it through the console. The result is recorded as a new
  revision (listed as *Roll back*) and audited as `stack.deployed` with mode `restore`. If the revision uses host
  access you need Admin, and if the stack was deployed again meanwhile the restore is refused and you review again.
- **Accept live into draft** (Operator, `stacks.update`). It makes what runs the stack's **next draft**: Bayplan
  rebuilds the compose file from the running services **on top of the deployed revision**, then you review and
  deploy it, which records a revision. It sends nothing to the engine. Because it starts from the revision, the
  secrets and configs the stack made stay declared as files, a value that equals a secret variable's is written as
  `${NAME}`, and an image keeps the text you wrote (`nginx:${TAG}`) while it is still the image that runs, so no
  secret is copied into `compose.yaml`. If a saved draft holds changes, a dialog asks **Replace the draft?**, naming who
  saved it and when (*Keep draft* is focused), because the draft, its files and variables are replaced and that
  cannot be undone. If somebody saved the draft after you opened the page it is refused (`draft_conflict`).
  It is audited as `stack.drift_accepted`. Note that a live value that is not a secret variable's, such as a key
  somebody set by hand on a service, is written into the draft as it is, where `stacks.read` holders can read it (see
  [threat-model.md](threat-model.md)).

### Reverts drift

When you review a deploy, an update or a roll back, anything it would **put back** is listed apart from your own
changes, in a section above the Summary: *This deploy reverts 2 changes made outside the stack*, with each setting,
what runs now and what the deploy sets, and the cause under it. It is a warning so nobody undoes a hot-fix by
accident, and it **never holds Deploy back**. A setting your draft changes itself, or one that already matches, is not
listed. If Bayplan could not check, the section says *Couldn't check for drift; the changes below are still accurate.*
There is no such section on a restore, an adoption or a first deploy, and none for a stopped stack.

## Git stacks

A **git stack** gets its compose files from a folder of a git repository instead of from the editor. The repository is
the source of truth: Bayplan reads it, shows you what changed, and deploys it through the same review, deploy console,
revisions and roll back as any other stack. It never writes to the repository. Stacks show their source in the list:
*Inline* (files kept in Bayplan), *Adopted* (taken over from something else) or *Git*.

- [Repositories and credentials](#repositories-and-credentials)
- [What a git stack imports](#what-a-git-stack-imports)
- [Variables, secrets and the committed `.env`](#variables-secrets-and-the-committed-env)
- [Checking for changes: polling and webhooks](#checking-for-changes-polling-and-webhooks)
- [Reviewing, pulling and deploying](#reviewing-pulling-and-deploying)
- [Auto-deploy](#auto-deploy)
- [Rolling back a git stack](#rolling-back-a-git-stack)
- [Changing the source, converting and detaching](#changing-the-source-converting-and-detaching)
- [Who may do what](#who-may-do-what-with-git)
- [Where the repository is kept: mirrors](#where-the-repository-is-kept-mirrors)
- [Git limits](#git-limits)

### Repositories and credentials

Git connections come in two objects, in two layers:

- A **credential** is an HTTPS token (with the user name the server expects) or an SSH deploy key. It is encrypted at rest
  and never shown again once saved; the page shows only the last four characters of a token or the fingerprint of a key.
- A **repository** is an address plus the credential you pick for it (or none, for a public repository). Bayplan never
  guesses a credential from the address: you choose it, and many repositories can share one.

Each object lives either on the **instance** (managed by instance administrators, usable in every environment) or on
**one environment** (managed by that environment's Admins, usable only there). A repository of an environment may use a
credential of its own environment or an instance credential. An instance repository can use only an instance credential.
The pages are **Git** in the environment's sidebar, and the instance-wide one for administrators.

The address must be `https://host/path`, `ssh://git@host/path` or `git@host:path`. Anything else is refused before a
connection is made: **`http://`** (it would send a token in clear text; use `https` or `ssh`), `git://`, `file://` and
a bare path. An address with a user name and password in it is refused too: add them as a credential. A redirect
from `https` to anything but `https` is not followed.

A repository can carry a custom certificate authority (PEM) for a self-hosted server with its own CA. There is no
option to skip certificate verification.

**SSH host keys are trusted on first use, by a person.** Bayplan ships the pinned host keys of github.com, gitlab.com
and bitbucket.org. For any other host, **Test** shows the key's fingerprint and you confirm it; Bayplan stores it on
the repository. From then on a different key is a hard error (*host key changed*), never accepted by itself, and
trusting the new key is an explicit, audited action by an Admin. An SSH address with no known key stops at the handshake,
before any credential is sent. Bayplan never reads the server user's `known_hosts` or `ssh_config`.

**Test** lists the branches and tags and reports whether the credential works. Saving never waits for a test and works
whether or not it passed; a problem shows when a stack uses the repository.

Every address Bayplan connects to goes through the same egress policy as registry calls (see
[threat-model.md](threat-model.md)): link-local, unspecified and multicast addresses are refused, and an operator can
refuse more ranges with `BAYPLAN_REGISTRY_BLOCK_CIDRS`.

### What a git stack imports

You pick, in order, a repository, a **ref** (a branch, a tag or a fixed commit), a **stack folder** and **one or more
compose files** inside that folder. Several compose files are merged in the order you list them, like
`docker compose -f a.yaml -f b.yaml`.

- A **branch** is followed. A **tag** is followed too, so a tag that is moved counts as a change. A **commit** is fixed
  and is never behind; to move it you change the stack's ref, which goes through review.
- The folder is the stack's root and its boundary. Nothing outside it is read.
- Only the files the stack needs are imported: the compose files, the folder's `.env`, and every file an `env_file`,
  a secret's `file:` or a config's `file:` names. Other files in the folder are listed as *not imported*, and a folder
  that breaks the [stack file limits](#limits) is refused with the file named.
- A symlink is followed only when it points at a file inside the folder; a link that leaves the folder is a blocking
  problem naming the path.
- **`include` and `extends` are blocked** on git stacks, with a message, the same as for every stack (see
  [What a stack cannot use](#what-a-stack-cannot-use)). Merge several files with the ordered list instead.
- **Submodules and Git LFS are not supported.** A file that is a submodule or a Git LFS pointer is a blocking problem
  naming the path, rather than a pointer file being deployed.
- Relative bind mounts stay unsupported, as for every stack.

There is no compose editor on a git stack, because the repository is the source: the files from the repository are
read-only in Bayplan. What you can edit in Bayplan are the variables and the **overlay files** below. The stack's
**Git** tab holds the rest of the settings.

### Variables, secrets and the committed `.env`

A `${VAR}` in the repository's compose files is found the same way as on any stack, and the form asks for the ones that
have no value. A value comes, in this order of precedence, from:

1. the stack's **variables table** (the strongest);
2. the **`.env` committed in the stack folder**;
3. the compose file's own default, `${VAR:-default}`.

The preview shows where each value comes from (*From the table*, *From the repo's .env*, *From the compose file's
default*, *Not set*).

**Do not commit secrets.** The repository is readable by everyone who can read it and by the git host. A key of the
committed `.env` whose name looks like a secret (a name with a `PASSWORD`, `PASSWD`, `PWD`, `SECRET`, `TOKEN`, `KEY`,
`APIKEY`, `CREDENTIAL` or `PRIVATE` part, such as `DB_PASSWORD`) raises a warning that names the key, never its value,
and advises setting a **secret variable** in Bayplan, which overrides it. Secret variables are sealed in Bayplan
and shown as `<hidden>`.

A secret that a compose file reads from a file (`secrets: db_pw: file: ./db_pw.txt`) is not in the repository either.
Add an **overlay file** at that path on the stack: it is stored in Bayplan, sealed like any secret stack file, and fills
a path the repository does not have. **A path that exists both in the repository and as an overlay file is a blocking
error**, never a silent shadow.

### Checking for changes: polling and webhooks

Bayplan compares the stack folder at the head of the ref with the deployed one. The status is one of:

- **In sync**: the folder's contents are the same. A commit that changes other folders of the repository does not make
  the stack behind and starts nothing, so a monorepo with a folder per stack does not redeploy every stack on every
  commit.
- **Behind (N)**: N commits changed the stack's folder since the deployed commit (it says *at least N* when the count is
  capped at 2,000 commits). **Review update** opens the review.
- **Error** with a reason: the repository could not be reached or refused the credential, the host key changed or is
  not trusted, the branch or tag is gone, the folder is gone, the new commit fails the checks (a missing variable, a
  blocked setting), the source differs from the deployed one, or an automatic deploy was held back or failed. The last
  good version keeps running. The status clears on the next success. Each new error is audited and appears on the bell.

A stack newly created, or one restored from a backup, reads *unknown* until its first check.

**Polling** is on by default, every 5 minutes, per stack (1 to 1,440 minutes; a fixed commit has nothing to check). Checks
are spread out with a little random delay, a failing stack backs off up to an hour, stacks that share a repository share
one listing of its refs, and at most four repositories are read at once, so a hundred stacks do not hit a git host at the
same second. **Keep polling on even with a webhook**: it catches a push the webhook missed. **Check now** checks at once.

**A webhook** tells Bayplan about a push straight away. Each git stack can have one. In the stack's **Git** tab choose
**Set up webhook**, pick the git host, and copy the address and the secret, which are **shown once**; a lost secret is
replaced with **Rotate secret**. The address is `{public URL}/hooks/git/{token}`, built on the instance's public URL
(Settings) or, when none is set, on the address you reached Bayplan at; your git host must be able to reach it. Then add it at the host:

- **GitHub**: in the repository open *Settings*, then *Webhooks*, then *Add webhook*. Payload URL: the address. Content
  type: `application/json`. Secret: the secret. Events: just the push event. Bayplan checks the
  `X-Hub-Signature-256` signature.
- **GitLab**: in the project open *Settings*, then *Webhooks*. URL: the address. Secret token: the secret. Trigger:
  push events. Bayplan checks the `X-Gitlab-Token` header.
- **Gitea or Forgejo** (and Gogs): in the repository open *Settings*, then *Webhooks*, then *Add webhook*, then *Gitea* or
  *Forgejo*. Target URL: the address. Secret: the secret. Trigger: push events. Bayplan checks the HMAC signature header.
- **Other (generic)**: send a `POST` to the address whenever the repository changes. Choose where the secret goes:
  **in the address** (the token in the address is the secret, and the request carries nothing else) or **in an
  `Authorization: Bearer <secret>` header**.

**A webhook only tells Bayplan to check.** It never deploys by itself: Bayplan ignores the body of the request, takes no
branch and no commit from it, and fetches the stack's own ref itself. Whether the check then deploys is a separate
setting (see [Auto-deploy](#auto-deploy)). Several pushes in a burst cost one check and one trailing check.

A request that names no webhook, one with a wrong signature and one for a paused webhook all get the same empty `404`,
so the address cannot be probed. A delivery is limited to **60 a minute per client address and 10 a minute per webhook**
(a burst of 10), and a body over **5 MiB** is refused; the body is only the input of the signature check. Behind a
reverse proxy the client address is the one the proxy reports, so set `BAYPLAN_TRUSTED_PROXIES` as described in
[reverse-proxy.md](reverse-proxy.md); if the table of client addresses is ever full (65,536 addresses at once), new
addresses are refused until it empties, and polling still catches the push. Accepted deliveries and rejected signatures
are audited (`git.webhook_delivered`, `git.webhook_rejected`) without the address, the token or the signature, and the
tab shows the last delivery.

A webhook acts on the authority of the **Admin who set it up**. If that person loses `git.manage` on the environment,
requests to it do nothing and the tab says so until an Admin takes it over from the user's page, or removes it.

### Reviewing, pulling and deploying

A new git stack waits for review by default. When it is behind, **Review update** (or **Pull & redeploy…**, which an
Operator may press) fetches the head, shows the normal [review](#the-review) against what is running, and deploys through
the console. The revision records the commit and the folder's tree hash.

- Image pinning follows [the usual rule](#image-pinning-and-update-images): only a service whose image text changed gets
  a fresh digest, so a push never silently pulls a newer `:latest`. **Update images** stays the explicit action.
- Overlay-file and variable edits are local changes handled by the normal review, not *behind*.
- Pinning problems found at a deploy are shown against `compose.yaml` only; that is a known limit of how git stacks
  report them.

### Auto-deploy

**Auto-deploy** is off for every new git stack and is opt-in per stack (*Deploying* card of the Git tab). When it is on,
a check that finds the stack behind pulls the head and deploys it **without a review**, on the same checks, operation
lock, revision and console as a person's deploy. It runs on the authority the person who turned it on holds **now**:
if they lose the permission to deploy on the environment, auto-deploy **pauses**, and if the stack uses host access
(bind mounts, privileges) they must hold Admin. The stack's Git tab and the bell say who it is paused for; an Admin can
take it over.

Auto-deploy never deploys when something blocks it. Instead the commit is held back, nothing is deployed, the stack shows
*error: held back* with the first problem, and the commit stays pending until a later commit fixes it, someone reviews
it by hand, or you press **Check now**. These hold a commit back:

- a blocking lint, policy or variable problem in the new commit (a missing variable, a blocked setting, a path clash
  between a repository file and an overlay file, `include`, a Git LFS pointer, a submodule);
- **unreviewed local edits**: a draft with changes nobody reviewed, or a new source waiting for review. Bayplan never
  deploys over them;
- the operation lock is held: that is not an error, and the next check tries again.

An automatic deploy goes ahead even when it **puts back changes made by hand** on the host: the repository is the source
of truth. The revision and the audit row list what was put back (for example *web replicas 5 → 3*), with sensitive values
withheld from readers who may not reveal them, and the bell notes it.

Auto-deploys are audited as `git.autodeployed` with the system actor (`system:gitsync` or `system:webhook`), the owner it
acted for, the commit and what started it.

### Rolling back a git stack

[Rolling back](#rolling-back-to-a-revision) restores a stored revision exactly, including its files and variables; it
needs neither the repository nor its mirror. A roll back **pauses auto-deploy**, so the repository's head is not deployed
straight back over the version you chose: the Git tab shows *Auto-deploy paused after rollback to #N*, and the head shows
as pending. Turn auto-deploy on again when the repository is fixed. The stack keeps following the same repository, ref and
folder, and a roll back does not change the draft.

### Changing the source, converting and detaching

- **Change source** (Git tab) edits the repository, ref, folder or compose files. It goes through review, so history is
  kept. Needs the permission to deploy.
- **Use a git repository…** points an **inline** or **adopted** stack at a repository. The first sync goes through review
  and is recorded as a revision. Existing variables are kept; the stack's own files are dropped unless you keep them as
  overlay files, and a path clash is a blocking error. Needs Admin.
- **Detach from git** (the *Advanced* section at the end of the Git tab, for Admins; not among the header actions) turns
  a git stack back into an inline stack. The files now deployed are copied into an editable draft, **replacing any draft
  the stack already had** (an unreviewed pull or an overlay edit is overwritten, and the dialog does not warn about it),
  polling stops, the webhook is removed, auto-deploy is turned off, and nothing running changes. It is audited as
  `git.stack_detached`. You can point the stack at a repository again later.

### Who may do what with git

Git has three permissions, listed in [permissions.md](permissions.md): `git.read` (view the repositories and credentials
that apply to an environment: Viewer and up), `git.manage` (Admin: manage the environment's repositories and
credentials, set up, rotate and remove webhooks, convert and detach stacks) and `git.instance` (instance administrators:
the instance's repositories and credentials). Creating a git stack needs `stacks.create` (Admin). Changing the polling
settings, turning auto-deploy on or off, changing the source and pulling need `stacks.deploy` (Operator). An instance
credential is **usable by every Admin of every environment** (it can be picked for their repositories), so give it
only the access a deploy needs: see [threat-model.md](threat-model.md).

### Where the repository is kept: mirrors

For each repository Bayplan keeps a bare **mirror** under `/data/git/<repository id>.git`, fetched one ref at a time and
read by commit ID; nothing is ever checked out, and the mirror holds neither the address nor the credential. A mirror is
only a cache:

- deleting a repository removes its mirror at once;
- a **daily sweep** removes a mirror that nothing has fetched or read for **14 days**, or that has grown past **2 GiB**,
  and the next check builds it again;
- a mirror is not in a [backup](backup.md), and a restored instance fetches it again on the first check;
- roll back never needs a mirror.

### Git limits

| What | Limit |
|------|-------|
| Files imported by a stack | the [stack limits](#limits): 50 files, 1 MiB each, 8 MiB in all |
| Polling interval | 1 to 1,440 minutes, 5 by default |
| One fetch | 10 minutes, and 1 GiB of response |
| "N commits behind" | counts up to 2,000 commits |
| Webhook delivery | 60 a minute per address, 10 a minute per webhook, body at most 5 MiB |
| Mirrors | removed after 14 days unused or over 2 GiB |

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

**The one exception is an [adopted stack](#adopting-a-stack).** Its networks and volumes were made before Bayplan
knew the stack, and a deploy reuses them as they are, so they keep the labels they had and **never get a
`bayplan.stack.id`**. They are the stack's by the names its record holds (see [What adopting
owns](#what-adopting-owns)). Its services and containers get the label at the first deploy.

| Label | On | Meaning |
|-------|----|---------|
| `com.docker.stack.namespace` | swarm services, networks, secrets and configs | The stack's name, as `docker stack deploy` writes it. A stack that Bayplan did not make but that uses the same name is a foreign stack. |
| `com.docker.compose.project` | standalone containers, networks and volumes | The stack's name, as Docker Compose writes it. |
| `com.docker.compose.service`, `.oneoff`, `.config-hash`, `.project.working_dir`, `.project.config_files` | standalone containers | Docker Compose's own labels. The working directory and config files carry neutral constants (`/bayplan-stack`, `compose.yaml`), never a path of the Bayplan server. A container whose config hash differs from its service is recreated. |
| `bayplan.stack.id` | services, containers, networks, volumes, secrets and configs; on a swarm also the volume options of a service's mount of a named volume | The ID of the Bayplan stack record that owns the object. Not on an adopted stack's existing networks and volumes. On a mount it is an ownership label, like the one on the service. A label alone never links an object to a stack. |
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
| Viewer (`stacks.read`) | See stacks, their files, reviews, deploys, resources, logs, history, drift, and unmanaged stacks with their generated compose and adopt check. Secret values stay hidden, and so do the environment values of services Bayplan did not deploy. |
| Operator (`stacks.update`, `stacks.deploy`) | Save and discard a draft; review; deploy, update images, roll back to any good revision, restore from drift, accept live into the draft, stop, start and cancel. |
| Env Admin (`stacks.create`, `stacks.delete`) | Create and remove stacks, **adopt an unmanaged stack**, and **deploy a stack that uses host access**. |
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
  services or containers with digest chips, replicas and ports, the last deploy and who made it, the deployed
  revision and its [drift](#drift), and banners for a running operation, a saved draft that is not deployed, a
  stopped stack, drift, and an [adopted stack](#the-first-deploy-of-an-adopted-stack) that Bayplan has not deployed
  yet. A failed last deploy is never hidden behind *Running* or *Degraded*.
- **History**: the [revisions](#history-and-roll-back) of the stack, with roll back.
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
  does not manage links to its read-only [unmanaged page](#unmanaged-stacks) (tooltip *Not managed by Bayplan.*).

## Limits

| What | Limit |
|------|-------|
| Stack name | swarm `^[A-Za-z0-9][A-Za-z0-9_-]{0,60}$` (no dot, 61 characters), standalone `^[a-z0-9][a-z0-9_-]{0,254}$` (255 characters), fixed for life; see [Stack names](#stack-names) |
| Revisions kept | 20 per stack by default, 5 to 200 (instance setting); the deployed and any running revision are always kept |
| Pasted compose file (adopt) | 1 MiB |
| Files | 50 per stack, 1 MiB each, 8 MiB in all; a path of at most 255 characters, a name in it at most 100 |
| Secret file | 500 KB on a swarm, 1,000 KB on a standalone host (config: 1 MB, 1,000 KB) |
| Secret variable | at least 4 characters |
| Convergence | each service `BAYPLAN_STACK_CONVERGE_TIMEOUT`, 10 minutes by default, 1 minute to 2 hours |
| Jobs | 5 running per person; one stack operation per environment |
| Checks | the editor's check has a 5-second deadline, and a load still running at the deadline is abandoned; 2 run at once per person |
| Adopt check, drift check, revision changes | each reads the engine, so 2 run at once per person (*Too many checks are running*) |
| Revisions listed | the newest 500 of a stack |
| Drift check | about every 5 minutes per stack, 3 seconds after a Docker event that names it |
| Ports | the `ports:` and `expose:` ranges of one compose file add up to at most 20,000 ports, counted after variables are filled in; a port is 0 to 65535 and a range runs upwards |
| Rendered YAML diff | bounded (20,000 lines, 4,000 edits) |
| Console output | the newest 2,000 lines |
| Logs | a stack's log follows up to 32 streams (a swarm's services, a host's containers), 8 log streams per person |
| Lists | at most 500 stacks come back in one list, after search and filters |

## What is recorded

Every stack action writes an audit row, with the stack and who did it and never a value: `stack.created`,
`stack.draft_saved`, `stack.draft_discarded`, `stack.deployed` (with its mode: deploy, update, rollback or restore; its
result, revision, job and service count, and for a roll back or restore the revision it restored), `stack.stopped`, `stack.started`, `stack.removed` (how many volumes were
removed and kept, and whether the definition was kept), and `stack.value_revealed` (the kind and name of the
variable or file shown, never its value), and `stack.adopted` (the stack, its target, the namespace or project it was
taken from, how many services it has and the revision it started with), `stack.drift_accepted` (the stack, the revision, the
draft version and the number of services) and, when the number of revisions kept
changes, `settings.updated` (the old and the new limit and how many revisions went). Opening and downloading a stack's logs write `logs.viewed` and
`logs.downloaded` with the stack as the target and none of the lines. Git adds `git.credential_created`, `git.credential_updated`,
`git.credential_deleted`, `git.repo_created`, `git.repo_updated`, `git.repo_deleted`, `git.repo_tested`, `git.host_key_trusted`,
`git.host_key_retrusted`, `git.sync_failed` (each new error of a stack's sync), `git.updates_changed`, `git.webhook_created`,
`git.webhook_rotated`, `git.webhook_removed`, `git.webhook_delivered`, `git.webhook_rejected`, `git.autodeploy_enabled`,
`git.autodeploy_disabled`, `git.autodeploy_paused`, `git.autodeployed` and `git.stack_detached`; none holds a token, a key,
a webhook secret or the webhook's address. See [access.md](access.md) for the audit log itself.

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
- **Text the Docker Engine or the Compose SDK writes while a deploy, stop or start runs is masked for whole secret
  values only.** The job's console and item details can show part of a value the engine refuses: for example, the
  name of a restart policy taken from a secret. This waits for a decision.
- **A network or a volume a file stops declaring is not removed** on either target, as `docker compose up` does
  not either; Remove deletes the networks the stack owns, and the volumes you tick. A roll back inherits this.
- **A secret or config that is declared and used by no service is not created.** `docker stack deploy` creates
  every declared secret and config; Bayplan creates the ones a service uses.
- **A changed secret or config on a standalone host reads "changed, 0 settings"** in the review, because its
  content hash is a Bayplan label the comparison leaves out, and the Other resources table lists no secret or
  config rows there.
- **A secret file of the largest size made of one repeated character can take minutes to check and review**, and
  one of about a megabyte of ordinary content takes seconds.
- **The Remove dialog's progress panel disappears** when the stack's own page turns into *This stack is gone*; the
  answer arrives as a toast.
- **The list of Portainer markers has not been checked against a real Portainer.** An unmanaged stack is flagged
  *Portainer* when a label key starts with `io.portainer.` or its Compose working directory is under
  `/data/compose/`. The working directory rests on one third-party note, and Portainer's own documentation describes its
  `io.portainer.` labels as access-control settings a person sets, not as marks of how a stack was made. Nobody has
  yet deployed a stack from Portainer CE, in swarm mode and in standalone mode, and compared. A stack Portainer made may
  carry neither marker and then lists as unmanaged without the Portainer advice, and a stack with a marker that
  Portainer does not use would be flagged wrongly. Adopt it the same either way. Removing the stack from Portainer
  *without deleting it* is still your step, and Bayplan never calls Portainer.
- **Compose's network and volume hashes are proved on two Compose versions only.** Adopting a standalone project
  relies on the hash Docker Compose stored on its networks and volumes matching the one Bayplan's engine computes for
  the same settings, so the first deploy leaves them alone. Real-engine tests made a project with Docker Compose
  2.29.1 (Engine 24) and 5.5.1 (Engine 29) and adopted it with no network or volume change. Projects made with other
  versions, such as an older v2 Compose or the one inside Portainer, have not been tried. If the hashes ever differ,
  the review shows the network as recreated (a brief restart) and the volume as kept, and a rebuild that finds no
  matching form keeps the object external.
- **A swarm's service events carry no stack.** Docker's service events name only the service, so Bayplan finds the
  stack of a changed service by the service's name.
- **A stack name of 61 characters works only with a short network name.** The 61-character cap is chosen so that
  `{stack}_{key}` fits swarm's 63-character limit for a one-character service. A stack that uses the implicit
  `default` network needs a name of at most 55 characters, and a longer one is stopped at the check
  (`swarm_name_invalid`).
- **Adopting copies environment values as literals.** Secret values in a service's environment end up in the stored
  draft and in revision #1, readable by `stacks.read` holders, until you move them to secret variables. *Accept live
  into draft* does the same for a value that is not a secret variable's (see [threat-model.md](threat-model.md)).

## What is not in this version

These are decided and planned for later phases. None of them is in this version.

- **Opening an old revision as a draft**, and comparing two arbitrary revisions. Roll back, then edit.
- **Dismissing a drift**, and any automatic fix. Drift is shown, and you restore or accept it.
- **Moving secrets out of an adoption for you.** Environment values are copied as literals and not sorted into secret
  variables, and the contents of existing swarm secrets and configs are referenced as external and never imported.
- **Removing an unmanaged stack without adopting it first**, and adopting services or containers that carry no stack
  label.
- **Two-way git sync.** Bayplan reads a repository and never commits to it; [git stacks](#git-stacks) are read-only
  apart from their variables and overlay files.
- **Update detection.** Nothing says an image has a newer version, and there is no fleet view of updates.
- **A janitor** for old objects (it will share the operation lock), and orphan rules for networks and volumes (they
  will treat [an adopted stack's recorded networks and volumes as owned](#what-adopting-owns)).
- **Variables shared across stacks or environments.**
- **A folder on the host, synced by the agent, for relative bind mounts.**
- **Delivering a secret variable as a Docker secret.**
- **Building images**, and `include` and `extends` (also on git stacks).
- **Submodules and Git LFS** in a git stack's folder.
