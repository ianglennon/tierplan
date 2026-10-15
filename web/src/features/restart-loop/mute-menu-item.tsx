// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { Needs } from "@/hooks/use-needs";

interface MuteMenuItemProps {
  envId: string;
  serviceId: string;
  name: string;
  muted: boolean;
  needs: Needs;
}

// Scaffold: the real item arrives with the GREEN commit.
export function MuteMenuItem(_props: MuteMenuItemProps) {
  return null;
}
