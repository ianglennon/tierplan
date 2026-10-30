<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<!-- SPDX-FileCopyrightText: 2026 The Coaming Authors -->

# Backups, restore and key rotation

A Coaming backup is one encrypted file with the extension `.coaming`. It holds everything you
would need to set Coaming up again on a new machine: users, teams, access, settings,
environments and the audit log. This page covers taking backups, restoring them in each of the
three ways, what happens to agents afterwards, and rotating the master key.

Backups are for Coaming's own data. They do not hold anything from the Docker hosts it manages,
and they are not an upgrade tool: see [upgrade.md](upgrade.md) for that.

## What a backup holds

**Included:**

- users, with their password hashes, and the tombstones of deleted users;
- teams, memberships and every access entry;
- settings;
- environments, with their certificate authorities and the records of the agent certificates
  they issued;
- the whole audit log;
- registry credentials, compose stacks, their revisions and the owned automations (see [stacks.md](stacks.md));
- **everything git**, in four tables: `git_credentials` (HTTPS tokens and SSH deploy keys, sealed), `git_repos` (address,
  credential choice, custom CA and the SSH host key a person trusted), `stack_git` (a git stack's repository, ref, folder,
  compose files, polling settings, auto-deploy setting and webhook, with the webhook's signing secret sealed), and the commit each revision of a git stack was made from;
- the same for anything added in later releases. A test fails the build if a table is neither
  included nor deliberately left out, so a new feature cannot silently miss the backup.

**Left out:**

- sessions, so a restore signs everyone out;
- invite links, password reset links and **pending agent join tokens**;
- **cached git mirrors** (the `git` directory of `/data`): they are a cache, and a restored instance fetches each one again
  on its first check;
- **`stack_git_state`**, the result of the last check of each git stack. It is derived, so every git stack of a restored
  instance reads *unknown* until it is checked again, which happens on its next poll or when you press **Check now**.

**Git secrets restore under the new key.** A git token, an SSH deploy key and a webhook's signing secret are opened with
the old key and sealed again under the passphrase-derived key when the backup is written, and again under the new
instance's key when it is restored; a webhook's routing token is kept only as a hash and comes back as it was, so the
address a git host already calls still routes to the stack as long as the instance's public URL is the same.

**The master key is never in a backup.** Before a backup is written, every stored secret is
re-encrypted under a key made from your passphrase, so the file restores onto an install that
has a different master key. This also means the key is not a substitute for the passphrase: you
need the passphrase to open the file, and you do not need the old key.

The file is encrypted with a key derived from the passphrase (argon2id) and an authenticated
cipher. **Coaming cannot recover a lost passphrase.** The passphrase must be at least 16
characters.

## Taking a backup

### From Settings

**Settings, Backup, Download a backup.** Type a passphrase twice and choose **Download backup**.
The file is named `coaming-backup-YYYYMMDD-HHMM.coaming` (UTC). Only instance admins can do this.

### From the command line

```sh
docker exec -i coaming coaming backup --out - > coaming-backup.coaming
```

The command reads the passphrase from the file named by `COAMING_BACKUP_PASSPHRASE_FILE`, or
from the first line of standard input. **It is never a flag**, because a flag shows in the process
list and in shell history. With `-i` and a pipe you can supply it on standard input:

```sh
printf '%s\n' "$PASSPHRASE" | docker exec -i coaming coaming backup --out /data/backups/now.coaming
```

Or keep it in a Docker secret or a mounted file and set `COAMING_BACKUP_PASSPHRASE_FILE` in the
container's environment. Without `--out` the file goes into the folder the scheduled backups use
(`COAMING_BACKUP_DIR` if it is set, else `backups/` in the data directory) and is named
`coaming-manual-YYYYMMDD-HHMMSS.coaming` (UTC). Two backups in the same second get `-2`, `-3` and so
on, and a name that is not free is never reused, so the command never replaces a file when it
chooses the name. `--out <file>` is your own name and does replace an existing file, as before.
`--out -` writes to standard output, so the file leaves the container without ever touching its
disk.

A manual backup is **not** a scheduled one. Retention never counts or deletes it, and
**Settings, Backup** does not list or offer it for download (that list and the download accept only
the scheduled name, which keeps the download from reaching any other file). Copy it off the host
yourself, for example with `docker cp`, and delete it when you no longer need it.

Coaming checks the audit log's chain first and refuses to back up a log that does not verify.
`--allow-broken-chain` overrides that, and the backup records that the chain did not verify.
Every backup is audited as `backup.exported`, by you in the web interface or by `system:cli`
on the command line.

## Scheduled backups

**Settings, Backup, Scheduled backups** writes an encrypted backup every day or every week at a
time you choose (UTC) and keeps the newest N, 7 by default.

- Backups are written to `<data>/backups/`, which is **inside the container's data volume**.
  Mount a volume or a host folder somewhere else and set `COAMING_BACKUP_DIR` to it, or mount it
  at `/data/backups`, to keep backups off the data volume. Nothing leaves the machine by itself:
  there are no S3 or SFTP targets in 0.x.
- Retention only ever deletes scheduled `coaming-backup-YYYYMMDD-HHMM.coaming` files in that
  folder. It never touches the pre-migration snapshots, pre-restore snapshots, manual backups
  made with `coaming backup` (`coaming-manual-*.coaming`) or other files.
- The schedule's passphrase has to be stored so backups can run unattended. It is encrypted with
  the master key. **Anyone who can read `/data` and the master key can read it**, and with it open
  the backups. That is a risk you accept by turning the schedule on. Use a passphrase that you do
  not use anywhere else.
- A failed scheduled backup raises an item in the header bell for instance admins ("The scheduled
  backup failed"), with the reason and when the last good one was.

## Restoring

There are three ways. All of them check the file and the passphrase **before** they change
anything, and all of them are audited into the restored log.

A restore signs everyone out, including you, because backups hold no sessions. Sign in with an
account from the backup. A restore from an older Coaming is upgraded as it is restored. A backup
from a newer Coaming is refused, like a downgrade.

### 1. The setup wizard on a new install

Open a fresh Coaming. After the setup token, choose **Restore from a backup** under the "Create
the admin account" step, upload the file and give the passphrase. Coaming shows what is in the
backup, you confirm, and it goes to the sign-in page. Use this to move Coaming to a new machine.

### 2. Settings, over the running instance

**Settings, Backup, Restore from a backup.** Choose the file, give the passphrase and **Check
backup**. Coaming shows what is in it: the instance, when it was made, the version and the counts.
Then **Restore backup** and type the instance's name to confirm. Only instance admins can do this.

Coaming saves a snapshot of the current database first as `backups/prerestore-<time>.db` in the
data directory, and keeps the newest three. The restore then replaces all state and restarts.

**How it restarts** depends on `COAMING_RESTORE_RESTART`:

- `exec` (the default): Coaming replaces its own process with a fresh one. The container keeps
  running, and no restart policy is needed.
- `exit`: Coaming stops with status 0 after the swap and relies on the container's restart policy
  (`unless-stopped` or `always`) to start it again. Use this if your platform does not allow a
  process to replace itself. **Without a restart policy, `exit` leaves Coaming stopped.**

If the restored database cannot start, Coaming **rolls back to the snapshot by itself** and says
so on the page. If a restore cannot finish, nothing is changed.

A reverse proxy in front of Coaming needs an **upload limit** at least as large as
`COAMING_BACKUP_MAX_UPLOAD` (default `1GiB`, from `1MiB` to `64GiB`), or the upload is cut off
and the page tells you to check the limit. Keep the page open while the
restore works.

If the public URL stored in the backup is not the address you are using, the summary says so.
After signing in, check **Settings, General**, or set `COAMING_PUBLIC_URL`.

### 3. The command line, with Coaming stopped

Use this when Coaming will not start, or you would rather not restore through the browser. It
works from a one-off container on the same volume:

```sh
docker stop coaming
docker run --rm -i -v coaming-data:/data ghcr.io/ianglennon/coaming:edge \
  restore --yes /data/backups/coaming-backup-20261001-0300.coaming
docker start coaming
```

Put the file on the volume first (or bind-mount it), and supply the passphrase on standard input
(`-i`) or through `COAMING_BACKUP_PASSPHRASE_FILE`. Use the same image you normally run.

Without `--yes`, the command only shows what the file holds and exits with status 2. It replaces
an existing database only with `--yes`, after saving it as `backups/prerestore-<time>.db`.

**The command refuses to run while Coaming holds the data directory.** The server keeps a lock on
`/data` for as long as it runs, and `restore` and `key rotate` take the same lock. The message
tells you to stop Coaming first. The lock is a file lock: network file systems such as NFS may not
honour it, so on those, always stop Coaming yourself before running either command.

## After a restore: agents

The certificate authorities and certificate records of every environment are restored, so
**agents enrolled before the backup reconnect by themselves** once your public URL points at the
restored Coaming. You do not reinstall them.

**Agents installed after the backup was taken** are unknown to the restored Coaming. It refuses
them with the reason `unknown_agent`, and says so in three places: the **Agents Coaming doesn't
know** card under **Settings, Environments**, the header bell, and the reason text on the
environment. To bring one back, choose **Show install command** on its row and run the new
command on the host. Show install command makes a **new join token**, because pending join
tokens are not part of a backup.

The list of unknown agents is kept in memory. It is empty after a restart until the agents try
again, and an entry drops off after 24 hours without a new attempt.

## After a restore: git stacks

Repositories, credentials, webhooks and the settings of every git stack come back, and the sealed secrets open under the
new key. Each git stack reads *unknown* until its first check, then *in sync* or *behind*; the mirrors are fetched again
on that check. Auto-deploy keeps its setting and its owner, so the first check of a stack that is behind may deploy it.
Check the **public URL** (Settings, General): the address a git host calls for a webhook is built on it, so on an
instance with a different public URL you add the new address at the git host, which **Rotate secret** shows.

## Rotating the master key

The master key encrypts stored secrets and keys the audit log's chain. Rotate it if you think it
leaked, or as routine. Rotation runs with Coaming **stopped**, as a one-off container:

```sh
docker stop coaming
docker run --rm -v coaming-data:/data ghcr.io/ianglennon/coaming:edge key rotate
docker start coaming
```

It makes a new key, re-encrypts every stored secret under it, re-signs the whole audit log, and
records a `key.rotated` entry that the old key vouches for. It prints the key IDs and counts and
**never the key itself**.

- The old key is kept as `coaming.key.old` next to the new `coaming.key`. Check the log with
  `docker exec coaming coaming audit verify`, then delete `coaming.key.old`. **Back up the new
  key file now.** Without it the stored secrets cannot be decrypted.
- **If you supply the key with `COAMING_ENCRYPTION_KEY`** (or `_FILE`), no key file is replaced.
  The new key is written to `coaming.key.new`. Set the variable from that file's contents, start
  Coaming, then delete the file. Until you do, Coaming refuses to start with the old value,
  because its key no longer matches the database.
- **Snapshots from before the rotation hold values sealed under the old key** and open only while
  `coaming.key.old` exists. That covers the pre-migration `coaming-*.db` files and the
  `prerestore-*` snapshots in `backups/`. Keep the old key as long as you might need to roll back
  to one of them. Backups made with `coaming backup` do not depend on the key, so they are
  unaffected.
- **If a rotation is interrupted** (a crash or a power loss), run the command again, or just
  start Coaming. It finishes or discards the earlier attempt before doing anything else.
- Like `restore`, it refuses while Coaming is running, and the same note about file locks on
  network file systems applies.

## Limits and things to know

- The backup passphrase must be at least 16 characters. It is never accepted as a flag, and it is
  never written to the audit log.
- The largest file the web interface accepts for a restore is `COAMING_BACKUP_MAX_UPLOAD`.
  `coaming restore` has no such limit.
- Keep **two things apart** and safe: the backup file and its passphrase, and, for a full
  recovery of the original data directory, `coaming.key`. A backup file restores without the old
  key. A copy of the whole `/data` volume holds both the key and the data they protect, so guard
  it like a password store.
- A backup is a copy at one moment. Anything done after it is lost by a restore, including
  users, access and audit entries.
- The key and the database go together when you copy the data directory instead of making a backup. A copy of
  `coaming.db` without `coaming.key` (or the key you supplied through `COAMING_ENCRYPTION_KEY_FILE`) holds the git tokens,
  deploy keys and webhook secrets only as ciphertext that nothing opens. A backup file does not have this problem: it
  restores under any key.
- Docker state is not in the backup. Coaming reads containers, images and the rest live from the
  hosts.
