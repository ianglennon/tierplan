// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { useT } from "@/i18n/use-t";

interface UsedByListProps {
  // The containers that use the network, volume or image, each with the node it runs on.
  containers: { name: string; node: string }[];
  // The swarm services whose specs mention it. A secret or a config also says how the service
  // mounts it: the file, owner and mode.
  services: { name: string; file?: string; uid?: string; gid?: string; mode?: string }[];
}

// UsedByList is the "in use by" content the network Delete callout, the volume callout and
// the image remove dialog share: one line for each container, with its node, and for each service.
// A service of a secret or a config carries its file, UID / GID and mode on the line. Names are
// the engine's own and render as text only. It renders nothing when nothing uses the object.
export function UsedByList({ containers, services }: UsedByListProps) {
  const t = useT();
  if (containers.length === 0 && services.length === 0) return null;
  return (
    <ul className="list-disc ps-5 text-sm">
      {containers.map((c) => (
        <li key={`c|${c.node}|${c.name}`} className="break-words">
          {t("usedBy.container", { name: c.name, node: c.node })}
        </li>
      ))}
      {services.map((s) => (
        <li key={`s|${s.name}|${s.file ?? ""}`} className="break-words">
          {s.file === undefined
            ? t("usedBy.service", { name: s.name })
            : t("usedBy.serviceMount", {
                name: s.name,
                file: s.file,
                owner: `${s.uid ?? ""} / ${s.gid ?? ""}`,
                mode: s.mode ?? "",
              })}
        </li>
      ))}
    </ul>
  );
}

// One service that mentions a secret or a config, with how it mounts it.
export interface ObjectUser {
  service: string;
  serviceId: string;
  stack: string;
  file: string;
  uid: string;
  gid: string;
  mode: string;
}

interface ObjectUsersTableProps {
  envId: string;
  // In the order the server sent them: by service name.
  users: readonly ObjectUser[];
  // "secret" or "config": the empty line says which.
  kind: "secret" | "config";
}

// The cells hold text from Docker. They wrap at any character, and the table scrolls sideways
// when the page is narrow.
const CELL = "align-top font-mono text-xs break-all whitespace-normal";

// ObjectUsersTable is the Used by tab of a secret or a config: each service whose spec mentions
// it, with its stack, the file name it mounts it as, the owner and group of the file and its mode.
// The services count even with no running tasks.
export function ObjectUsersTable({ envId, users, kind }: ObjectUsersTableProps) {
  const t = useT();
  const key = "usedBy.objects";
  if (users.length === 0) {
    return <p className="text-sm text-muted-foreground">{t(`${key}.empty.${kind}`)}</p>;
  }
  const missing = t(`${key}.missing`);
  return (
    <Table aria-label={t(`${key}.title`)}>
      <TableHeader>
        <TableRow>
          <TableHead>{t(`${key}.columns.service`)}</TableHead>
          <TableHead>{t(`${key}.columns.stack`)}</TableHead>
          <TableHead>{t(`${key}.columns.file`)}</TableHead>
          <TableHead>{t(`${key}.columns.owner`)}</TableHead>
          <TableHead>{t(`${key}.columns.mode`)}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {users.map((u) => (
          <TableRow key={`${u.serviceId}|${u.file}`}>
            <TableCell className={CELL}>
              <Link
                to="/env/$envId/services/$serviceId"
                params={{ envId, serviceId: u.serviceId }}
                className="rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
              >
                {u.service}
              </Link>
            </TableCell>
            <TableCell className={CELL}>{u.stack || missing}</TableCell>
            <TableCell className={CELL}>{u.file || missing}</TableCell>
            <TableCell className={CELL}>{`${u.uid} / ${u.gid}`}</TableCell>
            <TableCell className={CELL}>{u.mode || missing}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
