// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackDetail } from "@/api/stacks";

interface DeployBannerProps {
  stack: StackDetail;
}

// DeployBanner is the banner of a stack operation under way.
export function DeployBanner(_: DeployBannerProps) {
  return null;
}
