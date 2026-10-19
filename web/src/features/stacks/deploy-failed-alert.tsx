// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackDetail } from "@/api/stacks";

interface DeployFailedAlertProps {
  stack: StackDetail;
}

// DeployFailedAlert is the warning on a stack's page after a deploy that stopped (UI-SPEC "Stack page").
export function DeployFailedAlert(_props: DeployFailedAlertProps) {
  return null;
}
