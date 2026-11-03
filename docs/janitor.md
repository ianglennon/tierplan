<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# The janitor

The janitor clears away what Docker leaves behind: exited task containers that pile up when swarm tasks recycle, dangling images, and networks no stack owns any more. It runs on a schedule on every environment, shows you what it would remove before it removes anything, keeps a record of every run, and has one label, `tierplan.keep=true`, that keeps anything away from it. This page says what it does on its own, what it only tells you about, what it never touches, how to set it up and how to keep things from it.

The exact list of what each role may do is in [permissions.md](permissions.md), which is generated from the code. [networks.md](networks.md) covers the orphaned and unused networks the Address space page lists. [threat-model.md](threat-model.md) says what the janitor adds to the risks of running TierPlan, and [agent.md](agent.md) lists the calls an agent forwards for it.

- [What it removes on its own](#what-it-removes-on-its-own)
- [What it only reports until you opt in](#what-it-only-reports-until-you-opt-in)
- [What it never touches](#what-it-never-touches)
- [Defaults and the schedule](#defaults-and-the-schedule)
- [Policy levels](#policy-levels)
- [Keeping things from the janitor](#keeping-things-from-the-janitor)
- [What TierPlan does with the label itself](#what-tierplan-does-with-the-label-itself)
- [Previews and Clean now](#previews-and-clean-now)
- [Run history and retention](#run-history-and-retention)
- [Crash evidence](#crash-evidence)
- [The swarm task history limit](#the-swarm-task-history-limit)
- [Permissions](#permissions)

## What it removes on its own

Out of the box the janitor is on for every environment and runs for real: a scheduled run removes things, it is not a dry run. It removes these, and only these:

- **Exited swarm task containers beyond the newest few.** When a swarm task stops, Docker leaves its container behind. For each slot of a replicated service (for each node, for a global service) the janitor keeps the newest 2 exited containers and removes the older ones. The two it keeps are there so you can still look at why a task died. A container is removed only after it has been stopped for the grace period, counted from when it stopped, and only when the swarm no longer wants its task running.
- **Containers left by a stack that no longer exists.** On any environment, an exited container that carries the ID of a TierPlan stack that has been removed, whose compose project has no other container in any state, is a leftover and is removed after the grace period. So are the old one-off containers made by `docker compose run`, beyond the newest 2 of each service on a node.
- **Dangling images.** An image with no tag (shown as `<none>`) that no container uses, running or stopped, on that node, once it has been dangling for the grace period. Docker does not say when an image lost its tag, so TierPlan measures the grace period from the first time it saw the image without a name (or from when the image was built, if that is later). An image built long ago that has just been displaced by a newer pull or build of its tag is therefore kept for the whole grace period, and so is every dangling image the first time TierPlan looks after it is installed or restored from a backup. An image that gets a name again and later loses it starts a new grace period. Build cache is never touched.
- **Orphaned networks.** A network that a TierPlan or Docker stack made and whose stack is gone, once it has been idle for the grace period. This is the same list as the Address space page's orphans, judged by the same code (see [networks.md](networks.md)).

Each removal is made without force and without the container's volumes, through the agent of the node the object is on.

## What it only reports until you opt in

Some things look abandoned but may be somebody's. The janitor lists them in the preview as reported and does not remove them until you turn on the matching option on the environment's policy. Both options are off by default.

- **Also remove unused networks.** A network no stack ever owned, with nothing attached, once idle for the grace period. Until you opt in it is reported with the reason that the option is off.
- **Also remove stopped standalone containers.** A plain `docker run` container that someone stopped, and the lone stopped container of a compose project TierPlan does not manage (after `docker compose stop`, say), once stopped for the grace period. Someone may start these again, so until you opt in they are reported and kept.

## What it never touches

- **Volumes.** The janitor never removes a volume, and it never removes a container's volumes along with the container.
- **Running, paused, restarting, created or dead containers.** Only a container in the state `exited` is ever a candidate.
- **Anything younger than the grace period.** The grace period is 24 hours unless the environment says otherwise. It is the same value the Address space page waits before it calls a network orphaned.
- **Build cache** and images that are tagged or in use.
- **Anything labelled `tierplan.keep=true`**, anything a person has kept with **Keep**, and everything that belongs to a stack excluded from the janitor (see [Keeping things from the janitor](#keeping-things-from-the-janitor)).
- **Anything during a deploy.** A removal takes the environment's operation lock, the one a deploy takes, so a run and a deploy never overlap. A preview takes no lock.
- **A node that is not reporting.** The janitor skips what it cannot see. A node whose agent is offline is named in the run and its objects are left alone, and while any node is missing no network is judged at all, because the missing node may be the one that uses it.
- **An object it cannot place.** A container whose swarm labels are missing, a compose container whose stack TierPlan could not read, a container it could not inspect and a node whose clock differs from the server's by more than five minutes are listed as "can't tell" and left alone.

The janitor decides this on the server, from a fresh read of every node. The page only shows its decision.

## Defaults and the schedule

| Setting | Default |
| --- | --- |
| Janitor on | yes |
| Schedule | daily at 03:00, server time |
| Keep the newest exited task containers per slot | 2 (0 to 20) |
| Grace period | 24 hours |
| Also remove unused networks | off |
| Also remove stopped standalone containers | off |
| Run history kept | 90 days or the last 100 runs, whichever keeps more |

**Schedule.** The schedule has presets (hourly, every 6 hours, daily and weekly) and an advanced field that takes a five-field cron expression. A schedule that would run more often than every 15 minutes is refused when you save it. The page shows the next few run times before you save.

**Time zone.** The schedule is read in the server's time zone. TierPlan honours the `TZ` environment variable and carries its own time zone data, so a server in a container with no zone files still gets the right one. Around a clock change a time that does not exist that day is skipped, and a time that happens twice runs twice, an hour apart; the page shows exactly those times.

**Start offset.** So that many environments do not all start at 03:00, each environment starts a little after the slot, by an offset worked out from its ID that is the same every day and is at most 10 minutes (less for a frequent schedule). At most two environments are cleaned at the same moment.

**No catch-up.** If the server was down at the scheduled time, the run is not made up when it comes back. The next run is at the next slot. A run that was in progress when the server stopped is marked failed with the reason that the server restarted.

**Waiting for a deploy.** A scheduled run that finds the environment's lock held by a deploy waits up to 10 minutes for it. If the lock is still held it ends as skipped with the reason "deploy in progress" and tries again at the next slot. Clean now, which a person starts, is refused at once with "operation locked" instead of waiting.

## Policy levels

The settings are inherited from the widest level to the narrowest, and a narrower level can override a field or leave it to follow.

1. **Settings, janitor defaults.** The instance-wide defaults on the Settings page: every field of the table above and the run history retention. An instance admin changes them (`settings.write`). A change to a default reaches every environment that follows it, and the page says how many.
2. **The environment.** An environment's Janitor page shows its effective policy with each field marked Default or Overridden, and **Reset** sends a field back to following the default. Changing it needs `janitor.policy`. A standalone host is an environment, so this is also where a host's policy is set.
3. **A swarm node.** On a swarm, a node can override only whether the janitor is on for it and its keep-N. A node switched off is skipped by scheduled runs and by Clean now alike, and its containers and images are listed in the preview as protected. A node's keep-N replaces the environment's N for the containers on that node, while the slot is still ranked across all the nodes it ran on. Node settings are kept under the swarm's own node ID, so they survive an agent being reinstalled. Until a node's agent has reported which swarm node it is, TierPlan cannot tell which settings are its own, so while the environment has any node settings such a node is left alone: its containers and images are listed in the preview as protected, with a reason that starts "Can't tell yet". This happens only briefly, after an agent connects; with no node settings the node follows its environment.

The grace period is one setting per environment. It feeds both the janitor and the Address space page, so the two never disagree about when a network is old enough.

## Keeping things from the janitor

Put the label `tierplan.keep=true` on anything you want the janitor to leave alone. It is the one opt-out label for every kind of object the janitor judges: containers, images and networks. There is no second label for another kind of object. This is the label JAN-07 asks for.

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
- **On a swarm service, put it where the containers get it.** `labels:` under a service in a compose file reaches its containers, which are what the janitor judges. With `docker service create` use `--container-label tierplan.keep=true`. A label on the service itself (`--label`, or `deploy.labels:`) marks the swarm service and does not reach the exited containers.

### Keep

You cannot add a label to a container or an image that already exists. For those, every row of a preview and of the report lists has a **Keep** action. It stores a keep against the object, and the janitor leaves it alone from then on. The page lists everything that is kept in **Kept items**, with who kept it and when, and **Stop keeping** removes a keep. A keep that comes from a label cannot be undone here; remove the label. Keep and Stop keeping need `janitor.run` and are audited.

A few things to know about Keep:

- A keep is stored against the object, not a pattern. It names a container or an image by its ID on a node, and a network by its name (so a network that is created again under the same name is still kept). The container or image is a different object after a redeploy, and the new one is judged as itself.
- A keep on a container or an image is stored against the node as TierPlan knows it, but it is matched by the object's ID on any node: a container's ID is unique across hosts, and an image's ID is its content. If you remove a node's agent and install a new one, TierPlan sees a new node ID, and the keeps stored under the old one still protect the same containers and images; **Kept items** shows them on the node they are on now. Keeping an image keeps every copy of it, on every node.
- Keep works for networks too, and is the same keep the Address space page uses.

### Keeping a whole stack

To leave everything a stack owns alone, do not label each object. Exclude the stack in one of two ways:

- the stack's **janitor switch** on its Overview page in TierPlan (needs `janitor.policy`, audited), or
- `x-tierplan: {keep: true}` at the top level of the stack's compose file, which lets a stack kept in git declare it in the repository.

The label on its own never excludes a whole stack, and neither do the services' labels together; only these two do. The `x-tierplan` line is read from the stack's current draft, or from its last good revision if there is no draft, so it takes effect when you **save** the compose file, not when you deploy it. A stack kept this way cannot be released with the switch: the stack's page says that the compose file keeps it, and the line has to be removed from the file. If TierPlan cannot read a stack's definition, it holds that stack's objects back as "can't tell" instead of guessing.

## What TierPlan does with the label itself

TierPlan writes some labels of its own on everything a stack makes (`tierplan.stack.id` and the rest, listed in [stacks.md](stacks.md)). Your `tierplan.keep` is not one of them, and TierPlan treats it as yours:

- **Adopting a stack** that is already running keeps the label in the compose file TierPlan writes for it, on services, containers, networks and volumes.
- **Rebuilding a stack's file** from what is running keeps it too.
- **Drift** compares the label like any setting you wrote: if the stack's definition has `tierplan.keep=true` and a running service has lost it, the service shows as changed, so a label cannot disappear without it being shown.
- **Deploying** puts the label where you wrote it, as the text `true`: `labels:` on a service reaches its containers (the exited task containers the janitor reads), `deploy.labels:` reaches the swarm service, and `labels:` on a network reaches the network.

A label can keep an object off the janitor's list for as long as it stays on it. That is the point of it, and it is the same risk as any label you put on an object by hand: the object stays until you remove the label or remove the object yourself. The same is true of Keep and of a stack exclusion. Each one can hide an object that really is stale, which is why the page lists everything that is kept, with a reason, rather than leaving it out of sight.

## Previews and Clean now

The **Janitor** page, under **Resources** next to Address space, shows the environment's status (last run, next run), its policy, the preview, the run history and, on a swarm, the task history limit. The environment's dashboard has a small Janitor card that links to it.

**The preview** is a dry run. It reads every node fresh through the agents, judges with the environment's policy and lists, grouped by type and node, what the janitor would remove now, each with its reason ("exited task, 3rd oldest of service web slot 2", "stack foo no longer exists", "dangling, 4 days old"). Below it are the things it looked at and left, with the reason: protected, kept, held back by the grace period (with the time left), reported until you opt in, or can't tell. A preview removes nothing and takes no lock, so it never holds up a deploy; the only thing it writes is when it first saw each image without a name, which the image's grace period runs from. Anyone who can read the environment can see it.

When you edit the policy, the page previews the policy as you have it open, so you see what a save would remove before you save it. Turning the janitor on, or widening a rule, asks you to look at that preview first.

**Clean now** removes what you tick in the preview. You can untick single items or whole groups first. It answers at once and carries on if you close the browser. It takes the environment's lock, plans again from a fresh read, and removes only what is still removable at that moment, each item once; an item that has changed since the preview (it is running again, it is now kept, it is gone) is left and the run records why. If something uses an object, the engine refuses and the item is kept, which is counted as a safe result, not an error. Containers are removed first, then networks, each network by the same code the Address space page's delete uses: its full inspect is saved to the audit log (`network.inspect_saved`) before the single removal (`network.removed`), and the rows name the janitor as the source. Clean now needs `janitor.run`.

## Run history and retention

Every run, scheduled or started by a person, is recorded: how it started and who started it, when it began and ended, how it ended (completed, partial, skipped with a reason, or failed), the counts and the space freed, and every item it attempted with its type, node, reason and outcome (`removed`, `refused_in_use`, `skipped_protected`, `skipped_node_offline` or `error`). Each run also writes one summary row to the audit log. The run page follows a run live while it works.

A run is kept for 90 days or until it is outside the newest 100 runs of its environment, whichever keeps more. An instance admin changes both numbers on the Settings page (7 to 3650 days, 10 to 1000 runs); lowering either asks first and says how many run records the save will delete. Old runs are removed when the server starts and after each run, along with their crash evidence. A server that stops mid-run leaves a run that is marked failed with the reason that the server restarted when it starts again.

A run the hub refused because the admin already has the most jobs running is recorded as failed. A run's own page is the record: the Jobs list can show a run as failed or partial when the run record says completed, because a container that was in use or no longer stale counts as a skipped job there but as a safe result in the run.

## Crash evidence

Before it removes a container that ended with a non-zero exit code, the janitor saves what you would want to see after it is gone: the exit code, the error, whether the engine killed it for memory (`OOMKilled`), when it started and finished, its image and digest, and the **last 200 lines** of its log. It is saved with the run's item and shown on the run page. It is removed with the run.

- **Masking is best effort.** The log lines pass through TierPlan's redaction before they are saved: values of environment variables whose names look like secrets, the container's own secrets, the stack's secrets when TierPlan knows them, and text that looks like a token, key or password. A pattern cannot know every secret, and a program can write any text to its log, so a secret in a log line may be saved. The page says that values that look like secrets were masked, not that none are present.
- **Caps.** At most 200 lines and 32 KiB of log text are kept for one container (the newest lines that fit), and at most 200 containers' log text per run. A crashed container after that is still removed, with its facts and the note that the cap was reached.
- **If it cannot be saved, the container is kept.** A crashed container whose evidence could not be written is not removed; the item records that.
- **Who can read it.** Evidence is read from the run page with `containers.read` on the environment, the same permission that reads container logs. Anyone who can read a container's logs can read what was saved from them.

Containers that exited with code 0 have no evidence saved.

## The swarm task history limit

Docker's swarm keeps a number of finished tasks for each slot (5 unless someone changed it). That limit decides how many task records the manager keeps, and the containers on the nodes go with the records Docker removes. The janitor's keep-N is a different number: it is how many exited containers the janitor leaves on a node. The two are not tied together, and TierPlan never changes the limit on its own.

On a swarm environment the Janitor page shows the limit as the manager reports it and lets a person with `janitor.run` change it, from 0 to 1000. Setting it to 0 means Docker keeps no finished task; a limit of -1, which keeps every task forever, is shown but cannot be set from TierPlan. A change reads the swarm's settings and version from a manager, changes only that one setting, and sends one update through the manager's agent. It never rotates a join token or the unlock key and never retries: if the swarm changed in between, you are asked to try again. A change is audited with the old and the new value.

**Agents must be updated to change it.** The agent only forwards this call from the version that introduced it. Reading the limit works with an older agent, but saving a change through one answers that the agent on a manager must be updated first; see [agent.md](agent.md).

## Permissions

| Permission | Who has it | What it covers |
| --- | --- | --- |
| `janitor.run` | Admin of the environment | Clean now, Keep and Stop keeping, and the swarm task history limit |
| `janitor.policy` | Admin; an Operator only when the assignment carries it | Changing the environment's or a node's policy, the opt-ins, and a stack's janitor switch |
| `settings.write` | Instance admin | The janitor defaults and the run history retention on the Settings page |
| `environments.read` | Viewer and above | The status, the preview, the run history and the task history limit |
| `containers.read` | Viewer and above | Crash evidence |

Every change to a policy, a default, a keep or a stack switch is written to the audit log, and so is each run. The full table of roles is in [permissions.md](permissions.md).
