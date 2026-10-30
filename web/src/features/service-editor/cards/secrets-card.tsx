// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { FileRefsCard, type FileRefsKind } from "@/features/service-editor/cards/file-refs-card";
import type { CardProps } from "@/features/service-editor/cards/form-card";

const SECRETS: FileRefsKind = {
  kind: "secret",
  path: "TaskTemplate.ContainerSpec.Secrets",
  idKey: "SecretID",
  nameKey: "SecretName",
  ns: "secrets",
  // A secret is mounted under /run/secrets, as a file of its own name until the person says
  // otherwise.
  defaultFile: (name) => name,
  absolute: false,
};

// SecretsCard: the secrets the service mounts, one row each (UI-SPEC "Service editor" Secrets).
export function SecretsCard(props: CardProps) {
  return <FileRefsCard {...props} cfg={SECRETS} />;
}
