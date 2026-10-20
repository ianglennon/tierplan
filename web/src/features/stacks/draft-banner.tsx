// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackDetail } from "@/api/stacks";

interface DraftBannerProps {
  stack: StackDetail;
  envName: string;
}

// DraftBanner is the banner of a draft that has not been deployed.
export function DraftBanner(_: DraftBannerProps) {
  return null;
}
