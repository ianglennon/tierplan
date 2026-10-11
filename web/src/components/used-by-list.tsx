// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useT } from "@/i18n/use-t";

interface UsedByListProps {
  // The containers that use the network or volume, each with the node it runs on.
  containers: { name: string; node: string }[];
  // The swarm services whose specs mention it.
  services: { name: string }[];
}

// UsedByList is the "in use by" content the network Delete callout and the volume
// callout share: one line for each container, with its node, and for each service.
// Names are the engine's own and render as text only. It renders nothing when
// nothing uses the object.
export function UsedByList({ containers, services }: UsedByListProps) {
  const t = useT();
  if (containers.length === 0 && services.length === 0) return null;
  return (
    <ul className="list-disc ps-5 text-sm">
      {containers.map((c) => (
        <li key={`c|${c.node}|${c.name}`} className="break-words">
          {t("networks.usedBy.container", { name: c.name, node: c.node })}
        </li>
      ))}
      {services.map((s) => (
        <li key={`s|${s.name}`} className="break-words">
          {t("networks.usedBy.service", { name: s.name })}
        </li>
      ))}
    </ul>
  );
}
