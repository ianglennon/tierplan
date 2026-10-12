// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { RegistryType } from "@/api/registries";

// One registry type of the credential sheet (D-04): the host it fills, whether the
// person may change that host, and the i18n key of the line that says which token to
// use. Docker Hub and GHCR have one address, so their host is read-only; GitLab
// defaults to the hosted registry and is editable for a self-managed one; a generic
// v2 registry has no default and needs one.
export interface Preset {
  type: RegistryType;
  host: string;
  hostEditable: boolean;
  helperKey: string;
}

// The four presets, in the order of the Registry type toggle.
export const PRESETS: readonly Preset[] = [
  {
    type: "dockerhub",
    host: "docker.io",
    hostEditable: false,
    helperKey: "registries.sheet.helper.dockerhub",
  },
  {
    type: "ghcr",
    host: "ghcr.io",
    hostEditable: false,
    helperKey: "registries.sheet.helper.ghcr",
  },
  {
    type: "gitlab",
    host: "registry.gitlab.com",
    hostEditable: true,
    helperKey: "registries.sheet.helper.gitlab",
  },
  {
    type: "generic",
    host: "",
    hostEditable: true,
    helperKey: "registries.sheet.helper.generic",
  },
];

export function presetFor(type: RegistryType): Preset {
  return PRESETS.find((p) => p.type === type) ?? (PRESETS[0] as Preset);
}

// hostAfterTypeChange is the host the field holds after the type changes. Choosing a
// type fills the host, but only over a host that is still the old type's own: a host
// the person typed (a self-managed GitLab, a mirror) is theirs and stays.
export function hostAfterTypeChange(host: string, from: RegistryType, to: RegistryType): string {
  return host === presetFor(from).host ? presetFor(to).host : host;
}
