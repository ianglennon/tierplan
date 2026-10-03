// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export type ReasonVariant = "full" | "compact" | "tooltip";

export interface ReasonParams {
  node?: string;
  offset?: string;
  version?: string;
  engine?: string;
  endpoint?: string;
  host?: string;
  environment?: string;
  hostType?: string;
  envType?: string;
  command?: string;
  direction?: "older" | "newer";
}

export interface ReasonCommand {
  id: string;
  caption: string;
  label: string;
  command: string;
}

export interface ReasonDescription {
  text: string;
  hint: string;
  known: boolean;
  commands: ReasonCommand[];
}

interface ReasonTextProps {
  code: string;
  params?: ReasonParams;
  since?: string;
  variant?: ReasonVariant;
  lead?: string;
}

// STUB: replaced in the GREEN commit.
export function useReasonText(): (code: string, params?: ReasonParams) => ReasonDescription {
  return () => ({ text: "", hint: "", known: false, commands: [] });
}

// STUB: replaced in the GREEN commit.
export function ReasonText(_props: ReasonTextProps) {
  return null;
}
