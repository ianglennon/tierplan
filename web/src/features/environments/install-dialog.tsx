// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface InstallDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  mode: "add" | "rotate";
  envId?: string;
}

// STUB: replaced in the GREEN commit.
export function InstallDialog(_props: InstallDialogProps) {
  return null;
}
