// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { SpecValue } from "@/features/spec-editor/spec-format";

export interface ServiceFormProps {
  spec: SpecValue;
  onSpecChange: (spec: SpecValue) => void;
  mode: "edit" | "create";
  envId: string;
  onOpenSpec?: () => void;
  onValidityChange?: (valid: boolean) => void;
}

// RED stub: replaced in the next commit.
export function ServiceForm(_props: ServiceFormProps) {
  return null;
}
