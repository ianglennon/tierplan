// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackDetail } from "@/api/stacks";

interface StoppedAlertProps {
  stack: StackDetail;
  onStart?: () => void;
}

// StoppedAlert is the alert of a stack a person stopped.
export function StoppedAlert(_: StoppedAlertProps) {
  return null;
}
