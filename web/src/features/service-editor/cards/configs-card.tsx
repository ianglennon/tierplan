// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { FileRefsCard, type FileRefsKind } from "@/features/service-editor/cards/file-refs-card";
import type { CardProps } from "@/features/service-editor/cards/form-card";

const CONFIGS: FileRefsKind = {
  kind: "config",
  path: "TaskTemplate.ContainerSpec.Configs",
  idKey: "ConfigID",
  nameKey: "ConfigName",
  ns: "configs",
  // A config is mounted at /{name} until the person says otherwise.
  defaultFile: (name) => `/${name}`,
  absolute: true,
};

// ConfigsCard: the configs the service mounts, one row each (UI-SPEC "Service editor" Configs).
export function ConfigsCard(props: CardProps) {
  return <FileRefsCard {...props} cfg={CONFIGS} />;
}
