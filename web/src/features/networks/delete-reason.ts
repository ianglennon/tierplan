// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What is attached to a network, as the counts the list and the page both carry.
export interface Attached {
  name: string;
  builtIn: boolean;
  attachedContainers: number;
  attachedServices: number;
}

// inUseWhat words what uses a network: "2 containers", "1 service" or both.
export function inUseWhat(t: Translate, n: Attached): string {
  const parts = [
    n.attachedContainers > 0
      ? t("networks.list.attached.containers", { count: n.attachedContainers })
      : null,
    n.attachedServices > 0
      ? t("networks.list.attached.services", { count: n.attachedServices })
      : null,
  ].filter((p): p is string => p !== null);
  return parts.length === 2
    ? t("networks.list.inUseAnd", { first: parts[0], second: parts[1] })
    : (parts[0] ?? "");
}

// deleteReason is why Delete… is unavailable on a network, or undefined when it is
// available: who may comes first, then what the network is, then what still uses it.
// On the network's own page the in-use words do not send the reader to the page they
// are on.
export function deleteReason(
  t: Translate,
  needsReason: string | undefined,
  n: Attached,
  onOwnPage = false,
): string | undefined {
  if (needsReason) return needsReason;
  if (n.builtIn) return t("networks.delete.builtin", { name: n.name });
  if (n.attachedContainers === 0 && n.attachedServices === 0) return undefined;
  const what = inUseWhat(t, n);
  return onOwnPage
    ? t("networks.detail.inUse.reason", { what })
    : t("networks.list.inUseBy", { what });
}
