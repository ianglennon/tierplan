// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

interface ObjectSheetProps {
  envId: string;
  kind: "secret";
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// ObjectSheet is the create sheet of a secret. Not built yet.
export function ObjectSheet(_props: ObjectSheetProps): null {
  return null;
}
