<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The TierPlan Authors -->

# Team access

TierPlan has users, teams and three roles. This page explains who can do what, how to
give people access, and how to get someone back in when they have lost their password.
The exact list of what each role may do is in [permissions.md](permissions.md), which is
generated from the code and cannot drift from it.

## Instance admins

An **instance admin** is a flag on a user. It is not a role. Instance admins manage users,
teams, settings, backups and the full audit log, add and remove environments, and handle
join tokens. On every environment they hold Admin with every extra permission.

- The flag is set per user, on the user's page in **Settings, Access, Users**. It cannot be
  given through a team, so the list of people with full control is always one visible list.
- The first account you create at setup is an instance admin. So is the Admin of a database
  that came from before roles existed.
- **TierPlan always keeps at least one active instance admin.** The last one cannot be
  demoted, disabled or deleted. The server refuses it, and the buttons are disabled with the
  reason. Make someone else an instance admin first.
- An instance admin can use every part of TierPlan, including container shells and secrets.
  To separate duties, give people environment roles instead of the flag.

## Roles and what they allow

A role applies inside one environment, or inside every environment (see below). There are
three, and they are fixed in 0.x:

| Role | What it can do |
|------|----------------|
| **Viewer** | Read only. See everything on the environment and read logs. Environment variable values stay hidden. |
| **Operator** | Day-to-day operations: deploy, update and redeploy stacks, start, stop, restart and scale, and pull images. Everything a Viewer can do. |
| **Admin** | Everything an Operator can do, plus create and delete stacks, networks, volumes, secrets and configs, manage nodes and agents, manage the environment's enrolment, and give access on this environment. Holds every extra permission. |

[permissions.md](permissions.md) lists every permission against every role. There is no
roles page in the web interface.

## Extra permissions

Four powerful actions are not part of the Operator role. They are granted on the access
entry itself, so "Operator on production, with container shell" is a single row:

| Extra | Lets the person |
|-------|-----------------|
| Container shell | Open a terminal inside containers |
| Reveal secrets | Show the values of secrets and hidden environment variables |
| Delete volumes | Delete volumes and the data in them |
| Change janitor policy | Change what the janitor cleans up and when |

- An **Admin** always holds all four. An **Operator** holds only the ones ticked. A **Viewer**
  can never hold any: the server refuses a Viewer with extras.
- Holding the permission is enough. TierPlan does not ask for the password again. Every use is
  audited, and destructive actions ask you to type the resource's name.

## All environments, and how access adds up

An access entry is for one environment, or for **All environments**. All-environments access
covers environments you add later, and the interface labels access that comes from it as
inherited.

A person's access to an environment is the **union** of everything that applies to them:
their own entries, the entries of every team they are in, and any all-environments entry.
The highest role wins, the extras of every entry add up, and **there are no deny rules**.
Removing one source never takes away what another still gives.

The user's page has an **Access** card that answers "why can this person do that": one row per
environment with the role, the extras and every source ("Instance admin", "All environments",
"Team name" or "Direct").

Access takes effect on the next request. A person who loses an environment is refused at once,
and their open log and event streams for it close. Their sessions stay valid.

### Environments a person cannot see

An environment with no access for a user does not exist as far as that user can tell. It is not
in the switcher, the lists or the command palette, and its address shows **Page not found**,
the same as an environment that was never there. A person with no access to any environment
sees a page that says so and names the instance admins to ask. (That page shows the usernames
of all instance admins to any signed-in user.)

## Teams

Teams give the same access to several people at once. A team holds only a name and its
members. Create a team in **Settings, Access, Teams**, add members, then give the team a role
on an environment, from the team's page or from the environment's Access tab.

- Only instance admins create, rename and delete teams and change who is in them. This stops
  anyone from widening their own access through a team.
- **Deleting a team, or removing a member, shows what changes first**: which people lose which
  environment, worked out under the union rule above. The change is then one audit entry with
  that list attached. Members keep every other entry they hold.

## Environment Admins giving access

An environment Admin sees an **Access** tab on the environment's settings page
(**Settings, Environments, the environment, Access**). It lists every team and person with
access, and where each one's access comes from.

An environment Admin can give existing users and teams the Admin, Operator or Viewer role,
and the extras, **on that environment only**. They cannot:

- give all-environments access or make anyone an instance admin;
- change an instance admin's access;
- create users or teams, or change who is in a team.

Environment Admins also manage their own environment's enrolment: renaming it, updating agents
and switching auto-update, revoking, blocking and unblocking nodes, and adopting a changed
cluster. Adding or removing an environment and handling the join token stay with instance
admins.

The audit log shows an environment Admin the entries for the environments they administer,
including refused attempts there. It does not show sign-ins, user and team changes or settings
changes.

## Adding people

An instance admin creates a user in **Settings, Access, Users, Create user** with a username,
and optionally a display name and an email. Nothing uses the email yet, and **TierPlan sends no
email**.

- TierPlan then shows a **one-time invite link**. It is hidden until you choose Show link.
  Send it to the person yourself, privately, because anyone who has it can set the password.
  The dialog cannot show it again once closed.
- The link works once and expires after 72 hours. Change the time with `TIERPLAN_INVITE_TTL`
  (from `1h` to `720h`). The token sits after a `#` in the link, so it is not sent to TierPlan's
  server or a reverse proxy in the request, and it is stored only as a hash.
- The person opens the link, chooses a password (12 characters or more) and is sent to the
  sign-in page. The link never signs anyone in by itself.
- A link that was not used shows "Invite pending", or "Invite expired" after its time. Choose
  **Regenerate invite link** in the row menu for a new one. The old one stops working.
- Usernames are 3 to 32 characters (letters, numbers, dot, dash, underscore), not
  case-sensitive, and cannot be changed afterwards.

## Lost passwords

There is no "forgot password" page. An instance admin opens the user's page or the row menu and
chooses **Reset password**. Their sessions end, and TierPlan shows a one-time link to give them,
exactly like an invite link. The setting `TIERPLAN_INVITE_TTL` applies to it too.

If the person who lost their password is the only instance admin, use the command line on the
host:

```sh
docker exec tierplan tierplan user reset-password <username>
```

It ends their sessions and prints a one-time link. The link is built from the stored public URL
and base path. Without a stored public URL it prints `/invite#token=…` for you to open on your
TierPlan address. The command is recorded in the audit log as `system:cli`. If an account is
locked after too many failed sign-ins (five failures lock it for 30 minutes), use **Unlock** on
the user's page or `docker exec tierplan tierplan user unlock <username>`.

## Disabling and deleting

- **Disable** signs the user out everywhere and refuses their sign-in with the usual generic
  message. Their access and team memberships are kept, and **Enable** restores everything.
- **Delete** cannot be undone. The user's access, team memberships, sessions and pending links
  are removed. The username is kept as a reserved tombstone and can never be used again, so the
  audit log still reads "alice (deleted)" and nobody can pose as a former user. The dialog asks
  you to type the username.

## Sessions

Each person sees and ends their own sessions from the user menu: **Sessions** lists where they
are signed in, with Revoke for one and **Sign out everywhere else**. An instance admin can see a
user's sessions on the user page and end them all with **End all sessions**. Every revoke is
audited.

Changing a password, resetting one, disabling a user and restoring a backup also end sessions.

## Automations and ownership

Git syncs and webhooks (added in later phases) run with the permissions of the user who set them
up, and TierPlan checks those permissions again on every run. If the owner is disabled, deleted,
or no longer holds enough access, the automation **pauses** with a visible reason and an audit
entry. An instance admin can **transfer ownership** to another user who has enough access, from
the owner's page or when deleting the owner. The delete dialog lists what the user owns and lets
you reassign each item or leave it paused.

## Compose files that need Admin

A stack whose compose file asks for a host-escalation feature can be deployed only by an Admin
on that environment (or an instance admin). Operators are refused, and the error names the
offending keys and lines. The features are those that give a container more reach than a
normal one has over the host:

- `privileged`, `cap_add`, `devices` and `device_cgroup_rules`;
- `pid`, `ipc`, `uts`, `cgroup`, `cgroup_parent`, `userns_mode` and `network_mode` set to the
  host or another container;
- a `security_opt` that turns confinement off;
- volumes that mount the Docker socket or runtime sockets, the host's root or Docker data
  directories;
- any value that holds an interpolation TierPlan cannot resolve before deploying, because it
  could be any of the above.

The check is deliberately cautious: a false alarm costs an Admin one click, and a miss costs a
host. The stack screens that use it arrive in later phases; the rule and its policy hook are
already in place.
