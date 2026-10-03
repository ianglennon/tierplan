// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { ReactNode } from "react";

import type { EnvironmentItem } from "@/api/environments";

// RED stub: the real provider arrives with the GREEN commit.
export interface EnvStreamFrame {
  envId: string;
  at: string;
  environment?: EnvironmentItem;
  agentId?: string;
  node?: string;
  type?: string;
  version?: string;
  reason?: string;
  code?: string;
  mode?: string;
  ip?: string;
}

export interface EnvHealthEvent {
  name: string;
  id: string;
  data: EnvStreamFrame;
}

export type HealthLive = "connecting" | "live" | "reconnecting" | "polling";

export function EnvHealthStreamProvider({ children }: { children?: ReactNode }) {
  return children;
}

export function useHealthLive(): HealthLive {
  return "connecting";
}

export function useEnvHealthEvents(
  _envId: string | undefined,
  _handler: (event: EnvHealthEvent) => void,
): void {}
