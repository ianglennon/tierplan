// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EnvironmentItem } from "@/api/environments";
import type { SortDir, SortKey } from "@/features/environments/sort";

// RED stub.
interface Props {
  items: EnvironmentItem[];
  loading?: boolean;
  sort?: SortKey;
  dir?: SortDir;
  onSortChange: (sort: SortKey, dir: SortDir) => void;
  canFinishSetup?: (envId: string) => boolean;
  onFinishSetup?: (envId: string) => void;
}

export function EnvOverviewTable(_props: Props) {
  return <table />;
}
