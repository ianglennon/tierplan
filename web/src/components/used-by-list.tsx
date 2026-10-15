// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useT } from "@/i18n/use-t";

interface UsedByListProps {
  // The containers that use the network, volume or image, each with the node it runs on.
  containers: { name: string; node: string }[];
  // The swarm services whose specs mention it.
  // A secret or a config also says how the service mounts it: the file, owner and mode.
  services: { name: string; file?: string; uid?: string; gid?: string; mode?: string }[];
}

// UsedByList is the "in use by" content the network Delete callout, the volume callout and
// the image remove dialog share: one line for each container, with its node, and for each service.
// Names are the engine's own and render as text only. It renders nothing when
// nothing uses the object.
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
        <li key={`s|${s.name}`} className="break-words">
          {t("usedBy.service", { name: s.name })}
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
  users: readonly ObjectUser[];
  // "secret" or "config": the empty line says which.
  kind: "secret" | "config";
}

// ObjectUsersTable is the Used by tab of a secret or a config. Not built yet.
export function ObjectUsersTable(_props: ObjectUsersTableProps): null {
  return null;
}
