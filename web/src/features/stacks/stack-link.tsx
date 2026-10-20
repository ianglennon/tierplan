// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

interface StackLinkProps {
  envId: string;
  name: string;
  bayplanStackId?: string;
}

export function StackLink({ name }: StackLinkProps) {
  return <>{name}</>;
}
