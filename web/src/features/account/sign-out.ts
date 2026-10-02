// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRouter } from "@tanstack/react-router";

import { api, getUnauthorizedHandler, setCsrfToken, setUnauthorizedHandler } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// useSignOut is the one way the app ends a session, used by the user menu and
// by the signed-in bar on the pages outside the shell.
//
// The rule (WR-17): the user must never believe they are signed out while the
// server session lives. The browser leaves the signed-in state only when the
// server confirms, with a 2xx or with a 401 (the session was already gone). A
// 403, 429, 5xx or a network failure is reported with a toast and changes
// nothing: the CSRF token and the cache stay, the expired-session handler is
// armed again and the user stays where they were.
export function useSignOut(): () => Promise<void> {
  const t = useT();
  const router = useRouter();

  return async function signOut() {
    // A deliberate sign-out is not an expired session, so a 401 on the way must
    // not show the "session expired" notice. Keep the handler to put it back.
    const handler = getUnauthorizedHandler();
    setUnauthorizedHandler(null);

    let status: number;
    try {
      const { response } = await api.POST("/api/auth/logout");
      status = response.ok ? 204 : response.status;
    } catch {
      setUnauthorizedHandler(handler);
      toastError(t("common.errors.unreachable"));
      return;
    }

    if (status === 204 || status === 401) {
      setCsrfToken(null);
      await router.navigate({ to: "/login", replace: true });
      // Only now that the signed-in pages are gone: nothing they fetched stays
      // behind for the next person at this browser.
      queryClient.clear();
      return;
    }

    setUnauthorizedHandler(handler);
    toastError(t("common.mutationFailed"));
  };
}
