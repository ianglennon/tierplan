// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createElement } from "react";
import { toast } from "sonner";

// UI-SPEC "Toasts": success lasts 4 seconds and is announced politely, an error
// lasts 8 seconds and is announced assertively. The sonner region itself is
// polite, so the message carries its own live-region role.
export const SUCCESS_TOAST_MS = 4000;
export const ERROR_TOAST_MS = 8000;

export function toastSuccess(message: string): void {
  toast.success(createElement("span", { role: "status" }, message), {
    duration: SUCCESS_TOAST_MS,
  });
}

export function toastError(message: string): void {
  toast.error(createElement("span", { role: "alert" }, message), { duration: ERROR_TOAST_MS });
}
