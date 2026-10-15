// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { SpecValue } from "@/features/spec-editor/spec-format";

interface StackOwnedStateProps {
  envId: string;
  serviceId: string;
  name: string;
  stack: string;
  spec: SpecValue;
}

export function StackOwnedState(_props: StackOwnedStateProps) {
  return <div />;
}
