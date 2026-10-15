// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { NodeList } from "@/api/nodes";
import type { ServiceTasks } from "@/api/services";

// One task of the service the filter can pick, current or earlier.
export interface FilterTask {
  id: string;
  slot: number;
  node: string;
  state: string;
  ended: boolean;
}

// One node the service has tasks on: its role, and how many of those tasks have not ended.
export interface FilterNode {
  name: string;
  role?: string;
  tasks: number;
}

// What the filter popovers offer. loaded is false until the service's tasks have been read.
export interface FilterOptions {
  loaded: boolean;
  tasks: FilterTask[];
  nodes: FilterNode[];
}

export const NO_FILTER_OPTIONS: FilterOptions = { loaded: false, tasks: [], nodes: [] };

// filterOptionsFrom turns the service's tasks (slots with their history) and the swarm's nodes
// into what the popovers list.
export function filterOptionsFrom(_tasks?: ServiceTasks, _nodes?: NodeList): FilterOptions {
  return NO_FILTER_OPTIONS;
}

export interface TaskFilterBarProps {
  service: string;
  options: FilterOptions;
  tasks: readonly string[];
  nodes: readonly string[];
  stopped: boolean;
  onTasksChange: (ids: string[]) => void;
  onNodesChange: (names: string[]) => void;
  onStoppedChange: (on: boolean) => void;
}

// TaskFilterBar is the third row of a service's log toolbar: the Tasks and Nodes popovers, the
// Include stopped tasks switch and the chips of what is chosen.
export function TaskFilterBar(_props: TaskFilterBarProps) {
  return null;
}
