// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ImageUsedByItem } from "@/api/images";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What uses an image on one node: its containers there by name, and the swarm services whose
// spec names it. A container blocks removing the image on its own node only; a service spec
// blocks it on every node, because Swarm would pull it again (UI-SPEC default 7, D-28).
export interface NodeUse {
  containers: string[];
  services: string[];
}

// nodeUse reads one node's use out of the Used by answer.
export function nodeUse(items: readonly ImageUsedByItem[], nodeId: string): NodeUse {
  return {
    containers: items
      .filter((u) => u.kind === "container" && u.nodeId === nodeId)
      .map((u) => u.name),
    services: items.filter((u) => u.kind === "service").map((u) => u.name),
  };
}

// inUse says whether anything uses the image on the node.
export function inUse(use: NodeUse): boolean {
  return use.containers.length > 0 || use.services.length > 0;
}

// blockReason is why the image cannot be removed on a node, or undefined: "Used by old-cache",
// "Used by old-cache and 2 more", else "Used by service proxy".
export function blockReason(t: Translate, use: NodeUse): string | undefined {
  const [first, ...rest] = use.containers;
  if (first !== undefined) {
    const what =
      rest.length > 0 ? t("images.remove.more", { name: first, count: rest.length }) : first;
    return t("images.remove.usedBy", { what });
  }
  const [service, ...others] = use.services;
  if (service === undefined) return undefined;
  const what =
    others.length > 0 ? t("images.remove.more", { name: service, count: others.length }) : service;
  return t("images.remove.usedByService", { name: what });
}

// inUseWhat words what keeps an image in use: only the kinds that are there, "1 container,
// 1 service".
export function inUseWhat(t: Translate, containers: number, services: number): string {
  const parts: string[] = [];
  if (containers > 0) parts.push(t("images.used.containers", { count: containers }));
  if (services > 0) parts.push(t("images.used.services", { count: services }));
  return parts.join(", ");
}

// inUseCounts counts the containers and services among the users.
export function inUseCounts(items: readonly ImageUsedByItem[]): {
  containers: number;
  services: number;
} {
  return {
    containers: items.filter((u) => u.kind === "container").length,
    services: items.filter((u) => u.kind === "service").length,
  };
}
