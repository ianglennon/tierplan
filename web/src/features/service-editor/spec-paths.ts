// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

const isRec = (v: unknown): v is Record<string, unknown> =>
  typeof v === "object" && v !== null && !Array.isArray(v);

// imageOf is the image a service spec names, or an empty string for a spec that names none.
export function imageOf(spec: unknown): string {
  if (!isRec(spec) || !isRec(spec.TaskTemplate) || !isRec(spec.TaskTemplate.ContainerSpec)) {
    return "";
  }
  const image = spec.TaskTemplate.ContainerSpec.Image;
  return typeof image === "string" ? image : "";
}
