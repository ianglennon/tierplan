// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { UpdateInfo } from "@/api/updates";

type ImageParts = Pick<UpdateInfo, "registry" | "repository" | "tag">;

// canBeNamed says whether the update object names an image: one that cannot be checked has no
// registry, repository or tag.
export function canBeNamed(info: ImageParts): boolean {
  return Boolean(info.registry && info.repository && info.tag);
}

// imageRefOf is the full reference the ignore routes match on: registry host, repository and tag.
export function imageRefOf(info: ImageParts): string {
  return `${info.registry}/${info.repository}:${info.tag}`;
}

// imageTextOf is the image as a person says it: the repository and the tag, without the registry
// host and without Docker Hub's implied "library/".
export function imageTextOf(info: ImageParts): string {
  const repository = info.repository.replace(/^library\//, "");
  return `${repository}:${info.tag}`;
}
