// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

export interface AddOverlayDialogProps {
  existingRepoPaths: readonly string[];
  overlays: readonly string[];
  onAdd: (overlay: { path: string; content: string }) => void;
  onClose: () => void;
  title?: string;
  initialPath?: string;
}

export function AddOverlayDialog(_props: AddOverlayDialogProps) {
  return null;
}
