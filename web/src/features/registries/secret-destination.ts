// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { Credential, RegistryType } from "@/api/registries";

// The fields of the form that say where a stored secret would be sent.
export interface Destination {
  type: RegistryType;
  // The address the credential signs in to: the form's host, or the preset's fixed one.
  host: string;
  username: string;
  caPem: string;
  insecure: boolean;
}

// secretDestinationChanged says whether the form points the stored secret somewhere it was not
// stored for: a different host, type, username, certificate authority or insecure switch. This
// is the server's rule (registry.SecretDestinationChanged, CR-C01): the secret is write-only, so
// it may only go where its owner last typed it, and once the destination moves a freshly typed
// secret is required. Clearing the CA or switching insecure off counts too.
export function secretDestinationChanged(stored: Credential, now: Destination): boolean {
  return (
    now.host.trim() !== stored.host ||
    now.type !== stored.type ||
    now.username.trim() !== stored.username ||
    now.caPem.trim() !== stored.caPem.trim() ||
    now.insecure !== stored.insecure
  );
}
