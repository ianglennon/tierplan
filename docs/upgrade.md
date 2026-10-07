# Upgrading Bayplan

Upgrading Bayplan means replacing the container with one from a newer image and keeping the
same `/data` volume. Your users, environments, agents, settings and audit history all live in
that volume, so they carry over. This page covers both `docker run` and compose installs, what
Bayplan does when it starts on an older database, and how to go back.

Bayplan is on 0.x versions, so a minor release can change behaviour. Read the release notes of
the version you are moving to before you upgrade, and pin an `X.Y` tag (for example `0.2`)
rather than a floating tag.

## Before you upgrade

1. Read the release notes for every version between yours and the one you want.
2. Take a backup. If your version has `bayplan backup`, use it. Otherwise stop the container
   and copy `/data/bayplan.db` and `/data/bayplan.key` somewhere safe. Copy both: the key
   encrypts stored secrets and keys the audit log, and the database is unreadable in part
   without it. If you supply `BAYPLAN_ENCRYPTION_KEY` yourself, keep that value too.
3. Check free space. Bayplan writes a snapshot of the database before it changes it, and
   refuses to start if the volume has less than twice the database's size free.

## Upgrade with docker run

Use the same volume and the same environment variables as before. Only the image tag changes.

```sh
docker pull ghcr.io/ianglennon/bayplan:0.2
docker rm -f bayplan
docker run -d --name bayplan ... -v bayplan:/data ghcr.io/ianglennon/bayplan:0.2
```

Replace `...` with the flags you used the first time (the published port, the Docker socket
mount, your environment variables). Then check `docker logs bayplan` and confirm Bayplan
started without an error.

A volume that is not named or bind-mounted is lost when the old container is removed. Make sure
`/data` is a named volume or a bind mount before you run `docker rm`.

## Upgrade with compose

Change the image tag in your compose file, then:

```sh
docker compose pull
docker compose up -d
```

Compose recreates the container on the same volume. Check `docker compose logs bayplan`.

## What happens on start

When the new Bayplan finds a database from an older release it:

- Writes a snapshot first, to `/data/backups/bayplan-<old version>-<UTC time>.db`, before it
  changes anything. The newest three snapshots are kept.
- Refuses to start if the volume has less than twice the database's size free, instead of
  filling the volume.
- Applies the schema changes, then starts.

Set `BAYPLAN_SKIP_PRE_MIGRATE_BACKUP=true` to skip the snapshot and the free-space check. Only
do that when you already hold a backup.

Existing audit entries are never dropped or rewritten by an upgrade. If your database came from
a release where one global Admin role held all authority, that Admin becomes an instance admin,
and everyone else keeps what they had.

## After the upgrade

- Agents reconnect on their own. Give them a short grace period, then look at Settings,
  Environments: each environment should return to online without you touching the nodes.
- Check that the audit log is intact:

  ```sh
  docker exec bayplan bayplan audit verify
  ```

  It prints `audit chain OK` with the number of entries. A broken chain exits non-zero and
  names the first bad entry.
- Sign in and look at Settings to confirm your users and settings are as you left them.

## Rolling back

Downgrades are refused by design. A database written by a newer Bayplan stops an older one at
startup, naming both schema versions and the newest snapshot. There are no down-migrations.

To go back to the older version:

1. Stop the container.
2. Move the newest snapshot over the database: copy the newest file in `/data/backups` to
   `/data/bayplan.db`.
3. Delete `/data/bayplan.db-wal` and `/data/bayplan.db-shm` if they exist, so SQLite does not
   replay the newer database's write-ahead log over the restored file.
4. Start the older image on the same volume.

Anything saved after the upgrade started is lost by a rollback, because the snapshot predates
it. Keep `/data/bayplan.key` as it was.

## Version checks

Bayplan never contacts any server to look for, announce or download a new version. It does not
check for updates, and nothing in it phones home. Upgrading is always you replacing the image.
Watch the project's GitHub releases page for new versions.
