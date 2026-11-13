# Connecting a remote host or swarm: the TierPlan agent

A remote Docker host or swarm is managed through a small agent that you install on it. This page is for the person who runs those hosts: how to install the agent, what each flag does, how to rotate the join token, how to run it behind a proxy with its own certificate, what every connection problem means, and how to remove it again.

- [What the agent is](#what-the-agent-is)
- [What the agent forwards](#what-the-agent-forwards)
- [Before you install](#before-you-install)
- [Install on a swarm](#install-on-a-swarm)
- [Install on a standalone host](#install-on-a-standalone-host)
- [Rotate or disable the join token](#rotate-or-disable-the-join-token)
- [Host routes](#host-routes)
- [Settings on the host: read-only and self-update](#settings-on-the-host-read-only-and-self-update)
- [A proxy with its own certificate: TIERPLAN_AGENT_PIN](#a-proxy-with-its-own-certificate-tierplan_agent_pin)
- [Troubleshooting](#troubleshooting)
- [Cloned machines and the engine ID](#cloned-machines-and-the-engine-id)
- [Who can manage an environment's agents](#who-can-manage-an-environments-agents)
- [Revoke, unblock and enrol again](#revoke-unblock-and-enrol-again)
- [Updates](#updates)
- [Uninstall and manual cleanup](#uninstall-and-manual-cleanup)

## What the agent is

The agent is one container, `ghcr.io/ianglennon/tierplan-agent`, that runs next to the Docker socket of a host. **It dials out to TierPlan.** The host opens no inbound port and needs no Docker TCP socket, no TLS client certificates and no SSH key. TierPlan sends its Docker requests back over the connection the agent opened.

- On a swarm the agent is a global service, one task per node, so TierPlan can reach node-local data on every node and cluster data through any manager.
- On a standalone host it is a single container.
- The agent forwards only a short, fixed list of Docker calls and refuses everything else, whatever TierPlan asks (see [What the agent forwards](#what-the-agent-forwards)). Apart from those calls, the only changes it makes are to itself, when TierPlan asks it to update or uninstall (see [Updates](#updates)).
- An agent is identified by its own key. The join token only lets a host enrol the first time; after that the agent proves possession of its key and the token is no longer needed (see [Rotate or disable the join token](#rotate-or-disable-the-join-token)).

TierPlan's own host does not need an agent: it uses the Docker socket directly.

## What the agent forwards

The agent is the security boundary between TierPlan and the host's Docker socket. The socket is root-equivalent, so the agent does not trust TierPlan with it: every request is checked against one fixed table of method, path, query keys and body shape, and a request that is not in the table is answered with `agent_policy_denied` and never reaches Docker. The agent decides this itself, from the bytes it receives. A compromised TierPlan cannot widen the list.

**Reads** (any query):

- `_ping`, `version`, `info`, `events`.
- The lists the dashboard shows: containers, images, volumes and networks.
- The swarm lists: nodes, services and tasks.
- The inspect of one container, one network, one volume, one service (`GET /services/{id}`, only the `insertDefaults` key) and one swarm node (`GET /nodes/{id}`, no query key), and the state of one exec.
- A container's logs (`stdout`, `stderr`, `since`, `until`, `timestamps`, `follow`, `tail`, `details` only).
- A container's stats, one sample at a time (`GET /containers/{id}/stats`, only the `stream` key, which must be `0` or `false`, and the `one-shot` key). The agent never holds a stats stream open, so this takes no interactive slot. A sample holds the container's CPU time, memory, network and block I/O counters and nothing else; TierPlan turns two samples into rates and keeps neither.
- On a swarm manager, the logs of a service (`GET /services/{id}/logs`) and of one task (`GET /tasks/{id}/logs`), with `stdout`, `stderr`, `timestamps`, `follow`, `details`, `since` and `tail` only. There is no `until`: the engine ignores it on these routes. Both are long streams like a container's logs and take one of the agent's interactive slots.
- Disk usage for volumes (`system/df` with `type=volume`).
- Images, by repository, `repository:tag`, `repository@digest` or image ID:
  - `GET /images/{name}/json` (inspect), with only the `manifests` and `platform` keys the Compose SDK sends (`manifests=1`, and `platform` as the JSON of an OCI platform such as `{"architecture":"amd64","os":"linux"}`, or as `os/arch[/variant]`), and `GET /images/{name}/history`, with no query keys.
  - `GET /distribution/{name}/json`, with no query keys. It asks the registry whether this host may pull the image, using the credential in `X-Registry-Auth` if one is sent.

**Changes** (a read-only host refuses every one of these):

| What | Calls |
|------|-------|
| Container lifecycle | start, stop, restart, kill, pause and unpause (only the `t` timeout and `signal` keys), and remove (only `force` and `v`) |
| A shell in a container | create an exec, start it, resize its terminal. Start is the only call that may be upgraded to a raw two-way stream |
| Container create | `POST /containers/create`, only the `name` and `platform` keys. The body is the container spec the Compose SDK sends for a standalone stack (the `Config`, `HostConfig` and `NetworkingConfig` fields it fills), capped at 256 KiB; any other field is refused. A create that gives the container host access is refused unless the host set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` (see [Container creates and host access](#container-creates-and-host-access)) |
| Container rename | `POST /containers/{id}/rename`, only the `name` key (required, in the shape a container name has, at most 255 characters, no slash after the first character) and no body. The Compose SDK renames a container it has just created when it replaces one |
| Container archive | `PUT /containers/{id}/archive`, only the `path` (required, and only `/`), `noOverwriteDirNonDir` and `copyUIDGID` (only `0` or `false`) keys. The body must be a tar of exactly one regular file of at most 1,000 KB, written into a Compose container that is not running (see [Writing a stack file into a container](#writing-a-stack-file-into-a-container)). The Compose SDK injects a stack file secret or config this way, just before it starts the container |
| Networks | create, connect a container, disconnect a container, delete |
| Volumes | create, delete. A create of a `local` volume that binds a relative host path or one outside `/srv`, `/mnt` and `/media`, or that mounts a filesystem type other than `tmpfs` or a network share, and of any volume with a driver other than `local` (a volume plugin), is refused unless the host set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` (see [Service specs and host access](#service-specs-and-host-access)) |
| Images | pull (`POST /images/create`, only the `fromImage`, `tag` and `platform` keys, no body) and remove (`DELETE /images/{name}`, only the `noprune` key) |
| Services | create (`POST /services/create`, no query key), update (`POST /services/{id}/update`, only the `version`, `registryAuthFrom` and `rollback` keys) and remove (`DELETE /services/{id}`, no query key). Create and update take a service spec as their body and may carry `X-Registry-Auth`. A spec that gives tasks host access is refused unless the host set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true` (see [Service specs and host access](#service-specs-and-host-access)); so is a create or update that mounts such a volume by name, an inline volume plugin driver or a cluster volume |
| Secrets | list (`GET /secrets`), inspect (`GET /secrets/{id}`), create (`POST /secrets/create`) and remove (`DELETE /secrets/{id}`), none with a query key. The create body is a name, labels and a value of 1 to 511,999 bytes, capped at 1 MiB; a secrets driver, templating or any other field is refused. Docker never returns a secret's value, so neither does the agent. |
| Configs | list (`GET /configs`), inspect (`GET /configs/{id}`), create (`POST /configs/create`) and remove (`DELETE /configs/{id}`), none with a query key. The create body is a name, labels, content of 1 to 1,023,999 bytes and optionally the `golang` templating driver, capped at 2 MiB; any other templating driver, a driver option or any other field is refused. Docker returns every config's content with the list and with the inspect, and the agent forwards both as they are. |
| Swarm nodes | update (`POST /nodes/{id}/update`, only the `version` key) and remove (`DELETE /nodes/{id}`, no query key, so a node can never be force-removed). The update body is a node spec with a name, labels, a role (`manager` or `worker`) and an availability (`active`, `pause` or `drain`) and nothing else; labels are bounded like every other label map |
| The swarm's task history limit | update (`POST /swarm/update`, only the `version` key, which is required). The body is a swarm spec, and the agent forwards it only when it equals the swarm spec of the agent's own engine in every field except `Orchestration.TaskHistoryRetentionLimit`, which must be present and from 0 to 1000. What reaches Docker is the engine's own spec with that one field replaced, never the bytes the server sent. The `rotateWorkerToken`, `rotateManagerToken` and `rotateManagerUnlockKey` keys are refused, so a server cannot rotate a join token or the unlock key through an agent, or change the CA, raft, dispatcher, task default or auto-lock settings. It reaches only a manager's agent. An engine that does not answer is `agent_engine_unreachable` and the update is not forwarded. An agent from before this route answers `agent_policy_denied`, so update the agents before changing the limit |

**The Phase 6 routes at a glance.** All of them are matched on the method and the whole path (with or without the `/vX.Y` prefix), and a key not listed is refused. Writes are refused on a read-only host before the table is consulted.

| Route | Query keys | Body | Cap |
|-------|------------|------|-----|
| `GET /services/{id}` | `insertDefaults` | none | |
| `POST /services/create` | none | service spec; `X-Registry-Auth` allowed | 256 KiB |
| `POST /services/{id}/update` | `version`, `registryAuthFrom` (`spec` or `previous-spec`), `rollback` (`previous` or `none`) | service spec; `X-Registry-Auth` allowed | 256 KiB |
| `DELETE /services/{id}` | none | none | |
| `GET /services/{id}/logs`, `GET /tasks/{id}/logs` | `stdout`, `stderr`, `timestamps`, `follow`, `details`, `since`, `tail` | none | |
| `GET /nodes/{id}` | none | none | |
| `POST /nodes/{id}/update` | `version` | node spec: name, labels, role, availability | 64 KiB |
| `DELETE /nodes/{id}` | none, so `force` can never be sent | none | |
| `POST /swarm/update` | `version` (required) | swarm spec equal to the engine's own except the task history limit | 64 KiB |
| `GET /secrets`, `GET /secrets/{id}` | none | none | |
| `POST /secrets/create` | none | name, labels, value | 1 MiB |
| `DELETE /secrets/{id}` | none | none | |
| `GET /configs`, `GET /configs/{id}` | none | none | |
| `POST /configs/create` | none | name, labels, content, optional `golang` templating | 2 MiB |
| `DELETE /configs/{id}` | none | none | |
| `GET /containers/{id}/stats` | `stream` (only `0` or `false`), `one-shot` | none | |
| `POST /containers/create` | `name`, `platform` | container spec (Config, HostConfig, NetworkingConfig fields the Compose SDK sets) | 256 KiB |
| `POST /containers/{id}/rename` | `name` (required) | none | |
| `PUT /containers/{id}/archive` | `path` (required, `/` only), `noOverwriteDirNonDir`, `copyUIDGID` (`0` or `false` only) | a tar of one regular file, not JSON | 1,000 KB of content, plus 64 KiB for the whole body |
| `GET /images/{name}/json` | `manifests`, `platform` | none | |

There is no route to update a secret or a config (they are replaced by a new one), to read the swarm join tokens, or to attach to, export or read the files of a container. A container can be created and renamed, through the routes and the host policy above, and one file can be written into a stopped compose container.

For every call that carries a JSON body (exec create and start, network create, connect and disconnect, volume create, container create, service create and update, node update, swarm update, secret create, config create) the agent reads the JSON into Docker's own request type, refuses any field it does not know, builds a fresh request from the fields it allows, and forwards that fresh request. Whatever else the sender put in the body does not arrive. The one body that is not JSON, the tar of a container archive write, is checked as described below and forwarded as received.

**A registry credential crosses on four calls only.** `X-Registry-Auth` carries the credential TierPlan holds for a registry. The agent accepts it on an image pull, on the distribution check and on a service create and update, and deletes it from every other request before the request reaches Docker; `X-Registry-Config` is deleted from every request. On those calls the header must be absent or a single value of at most 16 KiB made only of base64url characters, otherwise the request is refused as `agent_policy_denied`. The agent never decodes, logs or keeps the value: a refusal is reported with the method, the path and the reason, without the query or any header.

**Refused, whatever TierPlan asks:**

- A **privileged** exec, an exec with **environment variables** or **detach keys**, an exec without a terminal, and a detached exec start.
- Any HTTP **upgrade** other than starting an exec. `attach` is refused, and so is reading a container's files (`GET` or `HEAD` on `archive`).
- A network that is an **ingress**, **config-only** or **config-from** network, a network driver other than bridge, overlay, macvlan or ipvlan, and IPAM options.
- A **forced** network disconnect.
- A **cluster volume** (a volume with a cluster spec).
- The legacy **`link`** key on a container remove, and **`force`** on a volume delete.
- Importing an image (`fromSrc`, `repo`, `message` and `changes` on a pull), **`force`** on an image remove, and tagging, pushing, saving, loading, building and pruning images.
- Changing a container, and creating one that gives the container host access, unless the host opted in (below); creating or changing a **service whose spec gives tasks host access**, or that mounts a host-access or volume plugin volume by name or inline, and creating a **volume** that mounts the host or names a volume plugin, unless the host opted in (below); writing into a running container, or into one Compose did not create, or anything but one regular file; the export and file contents of a container, and a stats stream that is left open (`stream=true`); the swarm join tokens (`GET /swarm`).
- A body larger than 64 KiB (256 KiB for a service spec or a container create, 1 MiB for a secret, 2 MiB for a config and 1,000 KB of content plus 64 KiB for a container archive), and a query larger than 4 KiB.

## Service specs and host access

A service spec is a whole container specification, so a service write could start a privileged container on every node. Unless the host owner set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`, a manager's agent refuses a service create or update whose spec holds any of these, answers `403` with `agent_policy_denied: host_policy:`, and TierPlan shows the refusal as `agent_host_policy`:

- A bind mount of the Docker socket or another container runtime socket, of `/`, or of any host path that is not under `/srv`, `/mnt` or `/media` (a path strictly below `/var/log` is allowed, `/var/log` itself is not), or a Windows named pipe. This is the same rule TierPlan applies to the bind mounts of a stack.
- A volume with the `local` driver that binds a relative path or such a path (`o=bind` or `type=none`: the engine passes the device to the kernel as given and resolves a relative one against its own working directory, usually `/`), or that mounts any filesystem type other than `tmpfs` or a network share (`nfs`, `nfs4`, `cifs`, `smb3`, `smbfs`, `ceph`, `glusterfs`): a host block device, an overlay whose options name host directories, or a host pseudo filesystem such as `proc`, `sysfs` or `devtmpfs` cannot be judged by a path (plan 06-50).
- A volume with a driver other than `local`, given inline in the mount (a volume plugin; its options cannot be judged), and a mount of a cluster volume.
- A mount of a named volume that already exists on the manager's engine with one of those backings. The agent asks its own engine once per write for each distinct named volume (`GET /volumes/{name}`) and refuses one whose driver is not `local` or whose options the rule above refuses. An anonymous volume, and a named volume the engine does not have, are judged by the mount's own options, because Docker creates them from those; a lookup the engine does not answer refuses the write, because Docker mounts an existing volume without asking; the agent answers it `agent_engine_unreachable` (502), as when the engine does not answer any request, not `host_policy` (plan 06-50).
- Any added capability (`CapabilityAdd`).
- SELinux disabled or the `spc_t` type, seccomp unconfined or custom, and AppArmor disabled.
- A network attachment to the host network, given by name or by ID. For an ID the agent asks its own engine for the network and refuses one of the `host` driver or named `host`; a network the engine does not answer for is not refused by the agent, because the engine refuses an unknown network itself.

The check runs after the read-only flag and after the body has been decoded strictly, so a read-only host refuses these writes first. Service writes reach only a manager's agent, so on a worker this check never runs; the volume-create rule below runs on every agent-managed host, swarm node or standalone.

**Without the opt-in, TierPlan cannot change a service that already has host access.** Every service update carries the whole spec, so scale, force update and roll back are refused for such a service as well. TierPlan's own agent service mounts the Docker socket; TierPlan does not scale it, and it is updated by the agent's own self-update (see [Updates](#updates)), not through this check.

**Volumes and host access.** A service can mount an existing volume by name, so every agent judges every volume create with the same path rule, and a manager's agent also inspects the named volumes of a service write (the bullets above). Unless the host set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`, it refuses a `local` volume that binds a relative host path or one outside `/srv`, `/mnt` and `/media`, or that mounts a filesystem type other than `tmpfs` or a network share, and a volume with any driver other than `local` (a volume plugin; its options cannot be judged), with the same `host_policy` answer, which TierPlan shows as `agent_host_policy`. NFS and SMB shares, tmpfs, a plain local volume and a host folder under `/srv`, `/mnt` or `/media` are forwarded. The manager's agent sees only its own engine, so a volume of the same name that exists only on another node is not judged (threat model residual risk 35). TierPlan lets only an environment **Admin** create volumes and writes the options to the audit log, and an allowed host-folder volume is still a bind of that path, so a service, or a container someone else starts, with it sees the folder. The server's own host is reached through the local socket, has no agent and none of these checks.

## Container creates and host access

The Compose SDK that deploys a standalone stack creates containers, so every agent forwards `POST /containers/create` through the same kind of check as a service write. The body is decoded strictly into Docker's own type and rebuilt from the fields the SDK fills; `ContainerIDFile`, `VolumeDriver`, `PublishAllPorts`, `Cgroup`, `Umask`, `OnBuild`, `NetworkDisabled` and the state fields of a network endpoint are refused, and the image must be named. Unless the host owner set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`, the agent also refuses a create that gives the container host access, answers `403` with `agent_policy_denied: host_policy:`, and TierPlan shows the refusal as `agent_host_policy`:

- `Privileged`, any added capability (`CapAdd`), a device, a device cgroup rule or a device request (a GPU).
- A network mode of `host` or `container:<id>`, or a user-defined network that is the host network by ID (the agent asks its own engine), and the same for each network the create joins.
- A pid, ipc, uts, user or cgroup namespace shared with the host (`host`), and a pid or ipc namespace shared with another container (`container:<id>`).
- A cgroup parent, `VolumesFrom`, any `MaskedPaths` or `ReadonlyPaths` (the empty list the SDK sends for `systempaths=unconfined` included), and a runtime other than `runc`.
- A security option other than `no-new-privileges`, the built-in seccomp profile, an AppArmor profile that is not `unconfined` and an SELinux label that neither disables SELinux nor names `spc_t`.
- A bind (a `Binds` entry or a bind mount) of anything that is not an absolute path under `/srv`, `/mnt` or `/media` (or strictly below `/var/log`), the Docker or another runtime socket included, a Windows named pipe, a mount of a volume plugin or a cluster volume, and a `local` volume (named, or given inline in a mount) whose options give host access, judged as for a volume create.
- A named volume that already exists on the engine with such a backing. The agent asks its own engine once for each distinct name; a volume that does not exist yet is forwarded, because the engine creates it as a plain local volume; a lookup the engine does not answer is `agent_engine_unreachable` (502).

A stack file's `network_mode`, `ipc` or `pid` set to `service:<name>`, and its `volumes_from`, reach the engine as a `container:<id>` mode and `VolumesFrom`, so they are refused in the same way: a standalone stack whose services share a namespace or volumes needs the opt-in on an agent host (the server's own host is reached through the socket and has no such check). The review lists such a stack as fine, because the manager does not count `service:<name>` as host access.

The shared vectors in `proto/hostpath/testdata/container-specs.json` pin these rules, and the agent's fuzz test judges every forwarded body against a second, independent rule. A read-only host refuses the create before the body is read.

## Writing a stack file into a container

A standalone stack can hand a container a secret or config that comes from a stack file. The Compose SDK does it between creating the container and starting it, with `PUT /containers/{id}/archive?path=/&noOverwriteDirNonDir=true`, and a body that is a tar of one file named for the path inside the container (`/run/secrets/db-password`). Every agent forwards that call, and only that call:

- The body is a tar of **exactly one entry, a regular file**. A symlink, hard link, device, FIFO or directory entry, a second entry, and anything but zero padding after the end-of-archive blocks are refused.
- The file's name is a **clean path**: relative, or with one leading slash as the SDK writes it, with no `..` or `.` segment, no `//`, no trailing slash and no NUL byte, and at most 4,096 bytes. The write is made at `/`, so the name is where the file lands in the container.
- The mode may use only the permission bits (`rwxrwxrwx`): **no setuid, setgid or sticky bit**. The entry has no link target, device number or extended (PAX) record, so it cannot carry file capabilities or extended attributes either.
- The content is **at most 1,000 KB** (the larger of the secret and config limits), and the whole body at most 1,000 KB plus 64 KiB.
- `copyUIDGID` may only be `0` or `false`, so the file is owned by root whatever the tar says. A secret or config that asks for another owner (`uid`, `gid`) is therefore refused on a standalone host.
- The container must exist, carry the `com.docker.compose.project` label and **not be running**. The agent asks its own engine (`GET /containers/{id}/json`) once, after the tar has passed; a container that is running, one without the label and one that does not exist are refused as `agent_policy_denied`, and an engine that does not answer is `agent_engine_unreachable` (502), never permission.

A read-only host refuses the call before the body is read. The bind mounts of a container are still judged by the container create rule above, so a file written below a bind lands where that bind was allowed to point. The container is checked once, so one that is started between that check and the write is not caught. The rule keeps the route from being a general way to write into live containers; it is not a boundary against a compromised TierPlan, which can stop a compose container and then write to it. What it cannot do is write a link, a device, a setuid file or a second file, or write into a container Compose did not create.

**The read-only flag comes first.** On a host with `TIERPLAN_AGENT_READ_ONLY=true` the agent refuses every request that is not a `GET` or `HEAD` before it looks at the table, so no table entry can reach a change, no shell can be opened (creating an exec is a `POST`), and no image can be pulled or removed. Image inspect, history and the distribution check are reads and still answer. The distribution check is a read for the agent but not for the network: it has the host's Docker engine connect to a registry host that TierPlan names, using the credential TierPlan sends, and the engine treats loopback and its `insecure-registries` hosts as plain HTTP. A read-only host therefore still lets the TierPlan server make that host's network position issue requests to registries, so "read-only" does not mean "makes no outbound connections". If that matters, firewall the host's egress. See [Settings on the host](#settings-on-the-host-read-only-and-self-update).

**An older agent refuses a standalone stack deploy.** An agent from before plans 07-02 and 07-04 answers `agent_policy_denied` to the container create, the container rename, the image inspect keys `manifests` and `platform`, and the archive write of a standalone stack deploy, so TierPlan cannot deploy a compose stack to a standalone host through it until it updates; TierPlan then says the agent needs an update (the same notice as below), and updates it itself unless the host set `TIERPLAN_AGENT_SELF_UPDATE=false` (see [Updates](#updates)).

**An older agent refuses the newer calls.** The table ships with the agent. An agent from before this list grew refuses container actions, logs, shells, networks, volumes, image pulls, removes, inspects and distribution checks, and service, node, secret and config calls, service and task logs and container stats with `agent_policy_denied` until it updates; TierPlan then says the agent needs an update, and updates it itself unless the host set `TIERPLAN_AGENT_SELF_UPDATE=false` (see [Updates](#updates)).

**An older agent forwards what this release refuses.** An agent from before plan 06-40 forwards every volume create, and one from before plan 06-50 forwards a `local` volume that binds a relative device or mounts a filesystem type other than `tmpfs` or a network share. TierPlan still lets only an environment Admin create volumes, but the agent's own check holds only once the agent updates (see [Updates](#updates)).

## Before you install

1. **TierPlan must be reachable from the host.** The agent dials `<agent URL>/agent/connect` over `wss://`. The agent URL is `TIERPLAN_AGENT_URL`, or the public URL (`TIERPLAN_PUBLIC_URL`) when it is not set. The install command shows the exact address under **Connects to**. If TierPlan is behind a reverse proxy, the proxy has to pass WebSocket upgrades and TierPlan has to trust its `X-Forwarded-Proto` header: see [Running TierPlan behind a reverse proxy](reverse-proxy.md).
2. **The agent must be able to check TierPlan's certificate.** Agents connect only over `wss://` and always verify the certificate; no setting skips the check, and the agent refuses a `ws://` address at start. TierPlan always serves HTTPS. With its own certificate (the default) every install command carries that certificate's pin (`TIERPLAN_AGENT_PIN`), so a direct connection needs nothing more. With certificate files you supply, or a public-CA certificate on a proxy in front, the agent trusts the certificate through the system roots and the command carries no pin. A proxy with a certificate from a private CA needs its own pin added; see [A proxy with its own certificate](#a-proxy-with-its-own-certificate-tierplan_agent_pin). TierPlan itself refuses a plain connection to the agent endpoint (`insecure_transport`), so a proxy must talk HTTPS to TierPlan.
3. **Docker Engine 24 to 29** on every host. Older engines are shown as unsupported.
4. **The host's clock must be right** to within five minutes (use NTP). TierPlan refuses an agent beyond that.

Add the host in TierPlan with **Environments, Add environment**. The dialog shows the install command once, with the join token in it. The command starts with a space, so shells that ignore space-prefixed commands keep the token out of their history.

## Install on a swarm

Run the command on **any manager** node. It creates the join token as a Docker secret and starts the agent as a global service:

```sh
 printf %s '<join token>' | docker secret create tierplan-agent-token-g1 - && docker service create --name tierplan-agent \
   --mode global --constraint node.platform.os==linux \
   --secret source=tierplan-agent-token-g1,target=tierplan-agent-token,mode=0400 \
   --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
   --mount type=volume,src=tierplan-agent,dst=/var/lib/tierplan-agent \
   --mount type=bind,src=/proc/1/net/route,dst=/host/route,ro \
   --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID \
   --update-parallelism 1 --update-delay 10s \
   --label tierplan.agent=true --container-label tierplan.agent=true \
   --env 'TIERPLAN_AGENT_URL=wss://tierplan.example.com/agent/connect' \
   [--env 'TIERPLAN_AGENT_PIN=sha256/...'] \
   --env 'TIERPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/tierplan-agent-token' \
   ghcr.io/ianglennon/tierplan-agent:<version>
```

(The dialog gives it as one line. The line breaks above are only for reading.)

| Part | What it does |
|------|--------------|
| `docker secret create tierplan-agent-token-g1` | Keeps the join token in a Docker secret instead of in the service definition. `g1` is the token's generation; every rotation creates the next name. |
| `--mode global`, `--constraint node.platform.os==linux` | One agent on every Linux node. A node you add to the swarm later enrols by itself: the service starts an agent there and the secret is already in place. |
| `--secret source=…,target=tierplan-agent-token,mode=0400` | Mounts the token at `/run/secrets/tierplan-agent-token`, readable by root only. |
| `--mount …docker.sock` | Gives the agent the Docker socket. This is root-equivalent on the host, which is why the agent is small, read-only and drops its privileges. |
| `--mount type=volume,src=tierplan-agent` | The agent's key, certificate, pins and the addresses it dials. It is what lets the agent keep its identity across restarts and updates. |
| `--mount type=bind,src=/proc/1/net/route,dst=/host/route,ro` | The host's IPv4 route table, one read-only file, so the subnet picker can warn about a LAN or VPN overlap (see [Host routes](#host-routes)). |
| `--read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID` | The agent's root file system is read-only and it has no Linux capabilities except the two it needs to drop from root to an unprivileged user at start. |
| `--update-parallelism 1 --update-delay 10s` | A rolling update changes one node at a time with a ten-second pause. |
| `--label` and `--container-label tierplan.agent=true` | How TierPlan finds the agent's service and containers, whatever the service is called. |
| `TIERPLAN_AGENT_URL` | Where to dial. |
| `TIERPLAN_AGENT_PIN` | Present only when TierPlan serves its own self-signed certificate: the pin that certificate is checked against. |
| `TIERPLAN_AGENT_JOIN_TOKEN_FILE` | The secret file the token is read from. |

**As a stack file.** The dialog's **Compose file** view gives the same service as a stack file. A stack file cannot create a secret from text, so its first line is the one-line command that creates the secret; run it first, then `docker stack deploy -c tierplan-agent.yml <stack>`. The service is then called `<stack>_tierplan-agent`, not `tierplan-agent`: the rotate, update and cleanup commands on this page name `tierplan-agent`, so change the name in them to match.

To see the agents on every node:

```sh
docker service ps tierplan-agent
docker service logs tierplan-agent
```

## Install on a standalone host

Run the command on the host:

```sh
 docker run -d --name tierplan-agent --restart unless-stopped \
   --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID --security-opt no-new-privileges \
   -v /var/run/docker.sock:/var/run/docker.sock -v tierplan-agent:/var/lib/tierplan-agent \
   -v /proc/1/net/route:/host/route:ro \
   --label tierplan.agent=true \
   -e 'TIERPLAN_AGENT_URL=wss://tierplan.example.com/agent/connect' \
   [-e 'TIERPLAN_AGENT_PIN=sha256/...'] \
   -e 'TIERPLAN_AGENT_JOIN_TOKEN=<join token>' \
   ghcr.io/ianglennon/tierplan-agent:<version>
```

The flags mean what they do on a swarm, with these differences:

- `--restart unless-stopped` brings the agent back after a reboot or a Docker restart.
- `--security-opt no-new-privileges` stops anything inside the container from gaining privileges.
- `-v /proc/1/net/route:/host/route:ro` is the same route-file mount as on a swarm (see [Host routes](#host-routes)).
- The join token is an environment variable here, so it stays visible in `docker inspect tierplan-agent` until you rotate the token (see below). Once the agent has enrolled the token is not needed again, so rotating it afterwards removes the exposure.

The **Compose file** view gives the same container for `docker compose -f tierplan-agent.yml up -d`.

```sh
docker logs tierplan-agent
```

shows what the agent is doing.

## Rotate or disable the join token

Settings, Environments, the environment, **Join token**:

- **Show install command** generates a new token and shows its command. **The old command stops working at once.** Agents that are already enrolled keep working: they use their own key, not the token.
- **Disable token** lets no new host enrol until you generate a new token.

For a swarm that already has agents, **Show install command** gives one command that swaps the secret instead of the install command: Docker secrets cannot be edited, so it creates the next secret, points the service at it and removes the old one:

```sh
 OLD=$(docker service inspect -f '{{(index .Spec.TaskTemplate.ContainerSpec.Secrets 0).SecretName}}' tierplan-agent) \
   && printf %s '<new token>' | docker secret create tierplan-agent-token-g2 - \
   && docker service update --secret-rm "$OLD" --secret-add source=tierplan-agent-token-g2,target=tierplan-agent-token,mode=0400 tierplan-agent \
   && docker secret rm "$OLD"
```

The swarm agents restart one at a time and reconnect.

A standalone agent keeps the token in its container's environment. To remove it from `docker inspect`, rotate the token in TierPlan and recreate the container with the new command (the `tierplan-agent` volume keeps the agent's identity, so the environment keeps its history).

### Run enrolled agents without the join token

Once the agents have enrolled (the node list shows them connected) the join token can come off the host. An enrolled agent proves who it is with its own key, so it keeps running, restarting and updating as the same agent without it.

On a swarm, on a manager, take the secret and the variable that points to it off the service, then delete the secret. `<N>` is the generation in the secret's name; `docker service inspect tierplan-agent` shows it:

```sh
 docker service update --secret-rm tierplan-agent-token-g<N> --env-rm TIERPLAN_AGENT_JOIN_TOKEN_FILE tierplan-agent
 docker secret rm tierplan-agent-token-g<N>
```

On a standalone host, recreate the container with the same flags and the same `tierplan-agent` volume, but without the `TIERPLAN_AGENT_JOIN_TOKEN` line. The manual update command the node list shows is exactly that: see [Updating and removing agents by hand](agent-updates.md).

What an agent without the token cannot do:

- A node that joins the swarm later cannot enrol. Its agent stops with "no join token".
- An agent that was revoked and then unblocked, or whose certificate expired, cannot enrol again by itself.

To put the token back on a swarm, take a new token from **Show install command** and add it to the service. Do not use the rotate command for this: it reads the current secret from the service and fails when there is none.

```sh
 printf %s '<join token>' | docker secret create tierplan-agent-token-g<N> - \
   && docker service update --secret-add source=tierplan-agent-token-g<N>,target=tierplan-agent-token,mode=0400 --env-add TIERPLAN_AGENT_JOIN_TOKEN_FILE=/run/secrets/tierplan-agent-token tierplan-agent
```

## Host routes

The subnet picker warns when a subnet you are about to choose overlaps a route on the host, such as your LAN or a VPN. To know the host's routes, every install mounts **one file** into the agent, read-only:

- a swarm service: `--mount type=bind,src=/proc/1/net/route,dst=/host/route,ro`
- a standalone container: `-v /proc/1/net/route:/host/route:ro`

The agent reads `/host/route` when TierPlan asks. Set `TIERPLAN_AGENT_HOST_ROUTE_FILE` to read it from another path. It reads only IPv4 routes, at most 512 of them, and nothing else about the host.

**Why one file and not `/proc`.** Mounting `/proc/1/net` fails, and mounting `/proc` would show the agent every process on the host. The single file is the smallest thing that answers the question. The agent keeps `--read-only`, no capabilities beyond the two it drops privileges with, and no host network.

**An agent you installed before this release has no such mount, and cannot get one by updating itself.** An update changes the image, not the mounts. The subnet picker then skips the check for that host and says it can't read the host's routes. To add the mount, run one of these, on a manager for a swarm and on the host for a standalone agent:

```sh
# swarm
docker service update --mount-add type=bind,src=/proc/1/net/route,dst=/host/route,ro tierplan-agent

# standalone: the same recreate as the update command, with the mount (add the TIERPLAN_AGENT_PIN line if your install had one)
docker pull ghcr.io/ianglennon/tierplan-agent:<version> && docker rm -f tierplan-agent && docker run -d --name tierplan-agent --restart unless-stopped \
  --read-only --cap-drop ALL --cap-add SETUID --cap-add SETGID --security-opt no-new-privileges \
  -v /var/run/docker.sock:/var/run/docker.sock -v tierplan-agent:/var/lib/tierplan-agent \
  -v /proc/1/net/route:/host/route:ro --label tierplan.agent=true \
  -e 'TIERPLAN_AGENT_URL=wss://tierplan.example.com/agent/connect' ghcr.io/ianglennon/tierplan-agent:<version>
```

A swarm update rolls through the nodes one at a time, with the ten-second pause the install set (`--update-parallelism 1 --update-delay 10s`), so the agents reconnect one by one. The standalone agent keeps its identity in its state volume, so it needs no join token and reconnects as the same agent.

**TierPlan's own host has no agent.** It reads the same file from its own container: add `-v /proc/1/net/route:/host/route:ro` to the `docker run` (or the same bind in a compose file) of TierPlan. Set `TIERPLAN_HOST_ROUTE_FILE` to read it from another path. Without the mount the subnet picker says it can't read this host's routes and skips that one check.

**Hosts that may refuse the mount.** Rootless Docker, a user-namespace remap (`userns-remap`), some SELinux or AppArmor profiles and Docker Desktop can refuse to bind-mount a `/proc` file, or show the agent an empty or different table. The agent then reports the file as unmounted. That is not an error: nothing else about the agent changes, and the picker says "the agent can't read the host's routes" and skips that one check.

## Settings on the host: read-only, self-update and privileged services

These are set by whoever owns the host, in the agent's environment (`-e` on a standalone host, `--env` on a swarm service). TierPlan cannot change them.

| Variable | Default | What it does |
|----------|---------|--------------|
| `TIERPLAN_AGENT_READ_ONLY` | `false` | The agent refuses every Docker request that is not a `GET` or `HEAD`, whatever the allowlist says. The node list shows **Read-only (set on host)**. On a read-only host TierPlan can still list and read, but it cannot start, stop or remove anything, create networks or volumes, or open a shell. It can still ask the host's engine to check that a registry image is reachable (the distribution check), which is an outbound connection. |
| `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES` | `false` | On a swarm manager, let the agent forward service specs that give tasks host access (and, on every agent-managed host, container creates that give the container host access, see [Container creates and host access](#container-creates-and-host-access)): a bind of the Docker socket or of a host path outside `/srv`, `/mnt` and `/media`, an added capability, a lowered sandbox, the host network (see [Service specs and host access](#service-specs-and-host-access)), and volumes that mount the host or use a volume plugin. Set it only on a swarm where you accept that a compromised TierPlan could then run a privileged service on every node, which is root on every node. It takes `true` or `false` (the other spellings Go's boolean parser reads, such as `1`, also work); any other value, a typo like `yes` included, stops the agent at start and names the variable, so the restriction is never dropped by mistake. On managers it covers service writes, including the volumes they name; on every agent-managed host it also covers volume creates. The server's own host has no agent; set it on the agent of every host where you need host-folder or plugin volumes outside `/srv`, `/mnt` and `/media`. |
| `TIERPLAN_AGENT_SELF_UPDATE` | `true` | Whether the agent may update or uninstall itself when TierPlan asks. With `false` the node list gives you the update command to run by hand and removing the environment leaves the agent in place (see [Updates](#updates) and [Uninstall and manual cleanup](#uninstall-and-manual-cleanup)). |

Smaller settings, rarely needed: `TIERPLAN_AGENT_SOCKET` (default `/var/run/docker.sock`), `TIERPLAN_AGENT_STATE_DIR` (default `/var/lib/tierplan-agent`), `TIERPLAN_AGENT_DOCKER_GID` (the group of the socket, when it cannot be detected), `TIERPLAN_AGENT_BACKOFF_MIN` and `TIERPLAN_AGENT_BACKOFF_MAX` (reconnect delay, 1 s to 60 s by default), `TIERPLAN_AGENT_HOST_ROUTE_FILE` (default `/host/route`, an absolute path to a read-only mount of the host's `/proc/1/net/route`; when the agent can read it, the handshake says so and TierPlan can ask for the host's IPv4 routes to warn about a subnet that collides with a LAN or VPN route; when it cannot, the check is skipped and nothing else changes).

## A proxy with its own certificate: TIERPLAN_AGENT_PIN

Agents use `wss://` only. The agent checks TierPlan's certificate in one of these ways and never skips the check:

1. **A certificate from a public CA** (Let's Encrypt, a commercial CA): nothing to configure.
2. **TierPlan's own self-signed certificate** (the default): the install command carries `TIERPLAN_AGENT_PIN`, the pin of TierPlan's key. The key is made once, is kept in the database and in every backup, and is never replaced by a re-signing, so the pin does not change, even across a restore onto a new host.
3. **A proxy with a certificate of its own CA** (a corporate CA, Caddy's internal CA, a self-signed certificate on the proxy): TierPlan cannot know that certificate. The pin in the command, if there is one, is TierPlan's own, which the proxy does not present, so the agent answers `tls_untrusted`. Add the proxy's pin to `TIERPLAN_AGENT_PIN`, separated by a comma from any pin already there.

A pin is `sha256/` and the base64 SHA-256 of a certificate's public key (its SPKI). Compute it from the certificate file:

```sh
openssl x509 -in cert.pem -pubkey -noout | openssl pkey -pubin -outform der | openssl dgst -sha256 -binary | base64
```

Put `sha256/` in front of the result and give it to the agent:

```sh
# standalone host: add before the image name
-e 'TIERPLAN_AGENT_PIN=sha256/<pin>'
# swarm: add to the service
docker service update --env-add 'TIERPLAN_AGENT_PIN=sha256/<pin>' tierplan-agent
```

Which certificate to pin:

- **The proxy's own certificate**, when it is a self-signed one: the agent accepts the server whose key matches.
- **A certificate authority that signed the proxy's certificate**, so a renewal of the proxy's leaf does not strand the agents. The agent accepts it only if the certificate the proxy presents really chains to the pinned authority for the address it dialled, and only certificates the proxy **sends** can be pinned. A proxy usually sends its leaf and its intermediate, not the root, so pin the **intermediate**. Caddy's internal CA works this way: pin `intermediate.crt` from `/data/caddy/pki/authorities/local/`, not `root.crt`.
- Several pins can be given, separated by commas, for the move from one certificate to the next.

A pin only adds trust. A certificate that a public CA signed is still accepted. A wrong pin and an unknown CA are refused with `tls_untrusted` and the agent's log shows the pin of the certificate it saw, so you can compare it with the one you meant:

```sh
docker logs tierplan-agent        # standalone
docker service logs tierplan-agent   # swarm
```

If the agent connects by an IP address (for example the proxy's address on a Docker network, because a host cannot resolve container names), the certificate must list that IP as a subject alternative name.

There is no switch that turns the check off, and none should be added to get around a certificate problem: fix the certificate or add its pin.

## Change TierPlan's agent address

Agents dial the agent URL (`TIERPLAN_AGENT_URL`, or the public URL when it is not set) with `/agent/connect` added. TierPlan accepts an agent only when the host and port it dialled are the current ones. A change of path only (for example a new base path) keeps them.

When you change the address in Settings, General:

- Every agent that is connected over `wss://` is sent the new address over its tunnel straight away, keeps its current connection, and dials the new address from its next connection. An `http` agent URL is refused, so there is no such move.
- Agents that are offline are not told. The confirmation counts the offline ones.
- From then on TierPlan refuses any agent that dials a different host or port, with `auth_rejected` and the audit reason `audience`. There is no fallback period. The agent keeps the previous address in its list for 30 days, but that helps only if you change the address back.

To get a refused agent back, either change the address back in Settings (the agent reconnects by itself), or start it again with the new address on its existing `tierplan-agent` volume, so it keeps its identity and needs no join token:

```sh
# swarm, on a manager
docker service update --env-add 'TIERPLAN_AGENT_URL=wss://<new address>/agent/connect' tierplan-agent
# standalone: remove the container, then run it again with the same flags and the same tierplan-agent volume,
# the new TIERPLAN_AGENT_URL and no join token (see "Run enrolled agents without the join token")
docker rm -f tierplan-agent
```

Show install command also works, but it makes a new join token and stops the old command, because it is a rotation. See [Run enrolled agents without the join token](#run-enrolled-agents-without-the-join-token) for how an enrolled agent starts without one.

The agent notices that `TIERPLAN_AGENT_URL` differs from the one it last started with and dials the new address first, also when it followed an earlier address change and was offline for a later one. An agent that last ran a version before this one notices from its first start on this version.

## Troubleshooting

Start with the agent's own log. It says what it tried and why it stopped, in one line per event, and it never prints the join token:

```sh
docker logs tierplan-agent               # standalone host
docker service logs tierplan-agent       # swarm, every node (add --tail 50, or look at one node with docker service ps)
```

TierPlan shows the same reason on the environment, in the overview's tooltip, in the environment's Connection card, and in the bell. Each reason has a code:

| Code | What it means | What to do |
|------|---------------|------------|
| `agent_offline` | The agent on a node is not connected. | Check that it is running (`docker ps` on a standalone host, `docker service ps tierplan-agent` on a swarm) and look at its log. It reconnects by itself when it can reach TierPlan. |
| `tls_untrusted` | The agent does not trust TierPlan's certificate. | Use a certificate from a public CA, or give the agent the pin: see [TIERPLAN_AGENT_PIN](#a-proxy-with-its-own-certificate-tierplan_agent_pin). With TierPlan's own certificate, reinstall with the command from Show install command, which carries the pin. |
| `auth_rejected` | TierPlan rejected the agent's credentials: a wrong, replaced or disabled join token, or an unknown agent. Every token problem looks like this to the agent, on purpose. It is also what an agent gets when it dials an address that is no longer TierPlan's agent URL, because it was offline when the address changed. | For a token problem, generate a new join token and reinstall the agent with the new command. The audit log entry's reason tells which it was: `audience` means the address, and [Change TierPlan's agent address](#change-tierplans-agent-address) says how to get the agent back. An enrolled agent that has a join token it has no record of being accepted with (you reinstalled it with the new install command, for example after TierPlan was reinstalled or restored to an empty data directory, or its identity was enrolled by an older agent, which kept no such record) sets its identity aside and enrols with that token. If TierPlan accepts the enrolment the agent is back within about a minute and drops the old identity; if TierPlan refuses it too, as it does for an address or proxy fault, the agent takes its identity back, so this refusal never costs it the identity. A token TierPlan refused that way is not tried again with the same identity, also after a restart (the agent keeps a hash of it, `tried-token.json`, next to its identity); start the agent with another join token, for example a newly generated one, to have it try that. An enrolment that failed for another reason (a blocked host, a busy server) is tried again a minute later. With the token on record it keeps its identity and keeps waiting. |
| `cert_expired` | The agent's certificate expired before it could renew (the host was off or cut off for a month). | Reinstall the agent with a fresh join token. It enrols again with a new certificate. While it can reach TierPlan an agent renews by itself, two thirds of the way through the certificate's life. |
| `revoked` | An admin revoked the node. | Remove the agent from the host. To let it back in, see [Revoke, unblock and enrol again](#revoke-unblock-and-enrol-again). |
| `blocked` | The node is blocked from enrolling. | Unblock it in Settings, Environments, in the nodes list. |
| `version_incompatible` | The agent and TierPlan are too far apart in version. | Update the agent by hand; the node list shows the command. See [Updates](#updates). |
| `clock_skew` | The node's clock is more than five minutes from TierPlan's. | Sync the host's clock (NTP). |
| `timeout` | The node stopped answering. | Check the network between the host and TierPlan. The agent reconnects by itself. |
| `not_a_manager` | Only worker nodes are connected, so TierPlan cannot read cluster data. | Check the agent on a manager node and that the manager is running. |
| `no_quorum` | The swarm has lost quorum. | Bring a manager back online. Node-local data keeps working. |
| `cluster_changed` | The agents now report a different swarm (it was re-initialised, or the host joined another one). TierPlan refused them and changed nothing. | Adopt the new cluster to keep the environment, or remove it. |
| `engine_unreachable` | The agent cannot reach Docker. | Check that `/var/run/docker.sock` is mounted into the agent and that the Docker daemon is running. |
| `unsupported_engine` | Docker on the node is older than Engine 24. | Upgrade Docker on that node. Its data is left out until then. |
| `agent_policy_denied` | The agent refused a request because it is not on its allowlist, or the host is set read-only. | The refusal is recorded in the audit log. If you did not expect it, check the TierPlan server and the agent's settings. |
| `agent_host_policy` | An agent refused a service spec that gives tasks host access (a manager's agent; this includes a mount of a named volume with a host path or a volume plugin) a container create that gives the container host access (any agent) or a volume create that mounts the host or names a volume plugin (any agent), because its owner did not set `TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true`. | Remove the host access from the service or the volume, or set the variable on that host's agent if you accept what it allows. |

An agent that is refused when it first connects also gets one of these. They appear in the waiting panel while you install, with the address the attempt came from, and in the audit log as `agent.enrol_refused`:

| Refusal | What it means | What to do |
|---------|---------------|------------|
| `duplicate_environment` | The host already belongs to another environment. | Open that environment, or remove it first, then enrol again. |
| `type_mismatch` | A swarm manager used a standalone environment's command, or the other way round. | Add the host as its own environment. |
| `host_mismatch` | A standalone environment already manages a different host. | Add this host as its own environment. |
| `host_already_connected` | The host already has an agent in this environment that is connected now: a second agent container or service on the host (for example a leftover `docker run` agent next to the global service, or an install command run again with another container name or volume), or a cloned VM. TierPlan refuses the new agent rather than replacing the connected one, so two agents on one host never replace each other. | Remove the duplicate agent from the host. The refused agent does not stop: it tries again once a minute and enrols by itself once the other agent is gone. An agent that was only stopping (its connection had not ended yet) is gone within about a minute, and no action is needed. |
| `insecure_transport` (agent log: HTTP 403 "did not arrive over TLS") | The agent's connection reached TierPlan without TLS. This is almost always a reverse proxy that talks plain HTTP to TierPlan, because TierPlan serves HTTPS only and does not take `X-Forwarded-Proto` as proof of TLS. | Point the proxy at TierPlan's `https://` port (HTTPS upstream) as described in [Running TierPlan behind a reverse proxy](reverse-proxy.md). |
| `rate_limited` | Too many connection attempts from one address, too many handshakes at once, or more than 16 handshakes at once from one address (an IPv6 /64 counts as one address). | The agent waits as long as TierPlan asks and tries again. |
| `internal_error` | TierPlan failed while handling a valid request. | The agent retries. Look at TierPlan's log. |

A few things that are not codes:

- **The environment stays "Waiting for the first agent" and no refusal is listed.** The agent never reached TierPlan's handshake. Look at the agent's log: a DNS problem (see the next item) or a routing problem, a proxy that does not pass the upgrade (TierPlan warns in the bell: "WebSocket connections are failing"), or an `insecure_transport` refusal that has no environment to show it under.
- **The agent cannot resolve TierPlan's host name, but the host can.** The agent runs on Docker's default bridge (the install commands set no network). Containers there get the host's `resolv.conf` without its loopback servers. When the host lists only loopback resolvers (Pi-hole, dnsmasq or unbound on `127.0.0.1`, or a Docker that itself runs in a container), nothing is left and Docker silently uses `8.8.8.8` and `8.8.4.4`, which cannot resolve LAN names. (systemd-resolved's `127.0.0.53` is handled: Docker reads `/run/systemd/resolve/resolv.conf` instead.) To tell: the agent's log shows a lookup failing "on 8.8.8.8:53", and the Docker daemon's log says "No non-localhost DNS nameservers are left in resolv.conf. Using default external servers". The agent image has no shell, so read the file on the host that `docker inspect -f '{{.ResolvConfPath}}' tierplan-agent` names. Fixes: on a standalone host, run the agent again with `--dns <your DNS server>`; on a swarm, `docker service update --dns-add <your DNS server> tierplan-agent`; or set `"dns": ["<your DNS server>"]` in `/etc/docker/daemon.json` and restart Docker; or give the agent an IP address as TierPlan's URL (with a certificate that lists that IP).
- **The agent stops at start and its log says `TIERPLAN_AGENT_URL` must start with `wss://`.** The address starts with `ws://` and not `wss://`, and the agent never dials it. Reinstall with the command from **Show install command**, which uses `wss://` and carries TierPlan's pin.
- **The agent exits and Docker restarts it again and again.** It exits on a refusal and Docker's restart policy brings it back after a longer pause each time, up to about a minute. Fix the cause in the table; the loop stops by itself.
- **An update or an uninstall answers `insecure_session`.** Both are accepted only on a session that checked TierPlan's certificate. An agent whose certificate check could not complete does not connect at all, so this is rare now: look for `tls_untrusted` in the agent's log first.
- **The agent cannot reach Docker** (the socket is not mounted, Docker is stopped, or the socket file is left over from a Docker that is no longer running). The agent cannot say anything to TierPlan before it has read its engine, so the environment stays pending with no reason of its own, and after two minutes the waiting panel adds the line that points you to the agent's log. The log says "the Docker engine did not answer on its socket"; start Docker, or mount `/var/run/docker.sock` into the agent, and it connects by itself.

## Cloned machines and the engine ID

TierPlan recognises a host by its Docker **engine ID**, which Docker stores in `/var/lib/docker`. A virtual machine cloned from another one **shares that ID**, so TierPlan sees the clone as the same host and refuses it as `duplicate_environment` (or, in a swarm, as a node it already has).

On the clone, give the Docker engine a new identity before you install the agent:

```sh
sudo systemctl stop docker
sudo rm -rf /var/lib/docker      # this removes every image, container and volume on the host
sudo systemctl start docker
```

If you cannot wipe `/var/lib/docker`, give the clone a clean Docker data directory. Swarm nodes cloned from one another also share a node ID and need `docker swarm leave --force` and a fresh join after the engine has its new ID.

## Who can manage an environment's agents

An **Admin** of an environment, and an instance admin, can rename it, switch its automatic agent updates, update agents, revoke, block and unblock its nodes and adopt a changed cluster. Those actions live under `/api/env/{envId}/...`, so an Admin of environment A is told nothing about environment B: it answers `404`, as if B did not exist. An Admin sees Settings with only the Environments tab, listing the environments they administer, and on each page the General, Connection, Nodes and agents and Agent updates cards.

Adding an environment, removing one, showing or rotating its join token and folding the local host into a swarm stay with instance admins. An Admin does not see the Join token, Local host or Remove environment cards. The full list is in [Permissions](permissions.md).

## Revoke, unblock and enrol again

Settings, Environments, the environment, the nodes list, **Actions, Revoke node**:

- The node is disconnected at once and its certificate is rejected for good. An admin revoke also **blocks** the node: it cannot enrol again until you unblock it. It is a good time to rotate the join token if the host may be compromised.
- **Unblock node** lets the host enrol again. The agent that was revoked is not revived. If it still has its join token (a standalone agent keeps it in its environment, a swarm agent in its secret), it discards its old identity and enrols again by itself, within about a minute, as a **new** entry. If it does not have the token, install the agent on the host again.
- A node that leaves a swarm (`docker node rm`) is revoked automatically after two checks, without a block, so it can enrol again if it comes back.
- After TierPlan is reinstalled, or restored to an empty data directory, it no longer knows the agents' identities and refuses them `auth_rejected`. Remove the old agent from each host, keeping its volume, as in [Reinstall an agent on a host that ran one](#reinstall-an-agent-on-a-host-that-ran-one), then run the new install command. Each agent then starts with the new join token on its old `tierplan-agent` volume, sets aside the identity TierPlan no longer accepts and enrols with the new token by itself within about a minute; once TierPlan accepts the enrolment the old identity is deleted. If the enrolment is refused the agent takes its old identity back; it tries the same token again a minute later only when the refusal was not `auth_rejected`, and a token refused `auth_rejected` is not tried again with that identity until the agent is started with another one. An agent started with the join token it has on record keeps its identity and keeps waiting, because that token cannot fix it. When an agent enrols again into the same environment from the same host, TierPlan retires the host's earlier agent entry (revoked with the reason `replaced`, without a block), so the node list does not show the host twice. It does that only while the earlier agent is not connected; while it is, the new enrolment is refused `host_already_connected` and the earlier agent is left alone.

### Reinstall an agent on a host that ran one

An install command creates the agent under fixed names, so it fails on a host that still runs an agent: on a swarm `docker secret create tierplan-agent-token-g<N>` refuses a secret that exists (every new environment starts at `g1`), and because the command chains with `&&` the service is not created either; `docker service create --name tierplan-agent` refuses an existing service, and on a standalone host `docker run --name tierplan-agent` refuses an existing container. Nothing is half installed, but nothing runs. Before you run a new install command on such a host, after TierPlan was reinstalled, after the environment was removed without **Also uninstall the agent from the hosts**, or to move the host to another environment, remove the old agent first and **keep its `tierplan-agent` volume**:

```sh
# swarm, on a manager: <N> is the generation in the old secret's name (docker secret ls --filter name=tierplan-agent-token lists it)
docker service rm tierplan-agent && docker secret rm tierplan-agent-token-g<N>

# standalone host
docker rm -f tierplan-agent
```

Then run the new install command. The agent starts on its old volume with the new join token and enrols again by itself, as described above; if TierPlan refuses the token it still has its old identity. Do not run `docker volume rm tierplan-agent` for this: the volume is what lets the agent come back by itself and keep its address list and pins. The secret names are the same for every environment, which is why the old secret has to go first.

## Updates

TierPlan updates agents itself after a server upgrade, within a compatibility window (the agent's minor version is the server's or the one before), unless you switch it off for the environment or the host sets `TIERPLAN_AGENT_SELF_UPDATE=false`. A swarm updates one node at a time through a manager; a standalone host updates through a short-lived helper container and keeps its identity. The whole story, including what to do when an update does not finish, is in [Updating and removing agents by hand](agent-updates.md).

Two things to know about a swarm update:

- A swarm rolls with Docker's own rolling update, and Docker's default when a task fails is to **pause**, not to roll back. If an agent image is broken, the first node's agent stops, the update pauses, and the other nodes keep the old agent. Put the service back with `docker service rollback tierplan-agent` (or `<stack>_tierplan-agent`). TierPlan shows the node as disconnected until you do, and after fifteen minutes lists "the agent never came back on the new version".
- A standalone update that cannot start the new agent leaves the old one running. The node list says "Docker refused the update" and the agent's log says why.

**Changing the swarm's task history limit needs an up-to-date agent on a manager.** The janitor's Janitor page can read and change how many finished tasks per slot the swarm keeps (its task history limit, [janitor.md](janitor.md#the-swarm-task-history-limit)). Reading it works through any agent. Changing it is a `POST /swarm/update`, a call an agent forwards only from the version that introduced it, so an agent from before that answers that its policy does not allow it, and the page says "Update the agent on a manager to change this limit." Update the agents (above) and try again. The agent still never lets the server rotate a join token or the unlock key, or change anything about the swarm except that one number.

Update and uninstall work only over a verified `wss://` session.

## Uninstall and manual cleanup

Removing an environment (Settings, Environments, the environment, **Remove environment**) revokes its agents and disables its token. Tick **Also uninstall the agent from the hosts** to have the agents that are online remove themselves: the service, its secret and the `tierplan-agent` volume on a swarm, the container and the volume on a standalone host. A swarm installed from a stack file leaves the stack's network behind (`<stack>_default`); remove it with `docker network rm`. Removal waits up to thirty seconds for the agents to answer, so a proxy in front of TierPlan must allow a response that takes that long.

Containers, images, volumes and networks of your own are never touched.

For an agent that was offline, or when you did not tick the box, run on each host:

Swarm, on a manager (the secret is named for the join token's current generation, which the environment's **Manual cleanup command** shows), then on every node that ran the agent:

```sh
docker service rm tierplan-agent && docker secret rm tierplan-agent-token-g1
docker volume rm tierplan-agent
```

Standalone host:

```sh
docker rm -f tierplan-agent && docker volume rm tierplan-agent
```

A stack-file install: `docker stack rm <stack>`, then remove the stack's network and the `tierplan-agent` volume if they are left behind.
