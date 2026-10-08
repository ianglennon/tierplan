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

// toastSuccessWithAction is a success toast with one button, such as "Rotate
// join token" after a revoke. A person needs time to read the message and then
// decide to press it, so it stays as long as an error does.
export function toastSuccessWithAction(message: string, label: string, onClick: () => void): void {
  toast.success(createElement("span", { role: "status" }, message), {
    duration: ERROR_TOAST_MS,
    action: { label, onClick },
  });
}

// toastInfo is a neutral message the person needs time to read, such as why an
// action is unavailable (D-19). It stays as long as an error does.
export function toastInfo(message: string): void {
  toast.info(createElement("span", { role: "status" }, message), { duration: ERROR_TOAST_MS });
}
