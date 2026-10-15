// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ServiceActionError } from "@/api/services";

// How long, after an update was accepted, the page waits for the swarm to start it before it
// reads the update once more. An object so a test can shorten it.
export const applyTiming = { settleMs: 2000 };

interface ApplyProgressProps {
  envId: string;
  serviceId: string;
  name: string;
  status: "pending" | "success" | "error";
  error?: ServiceActionError;
  // Whether the image is looked up before the update: it is when the image changed.
  lookup: boolean;
  onBack: () => void;
  onClose: () => void;
  onReload: () => void;
}

export function ApplyProgress(_props: ApplyProgressProps) {
  return <div />;
}
