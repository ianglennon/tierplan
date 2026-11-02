// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { redirect, useRouter } from "@tanstack/react-router";
import { useEffect } from "react";

import { setCsrfToken, setUnauthorizedHandler } from "@/api/client";
import { loadInstance } from "@/api/instance";
import { queryClient } from "@/api/query-client";
import { meQueryOptions, UnauthenticatedError } from "@/hooks/use-me";

// requireSession is the beforeLoad guard of every page that needs a signed-in
// person: the app layout and the pop-out windows, which sit outside it. A fresh
// install goes to the wizard first, and a visitor without a session goes to sign-in
// with the page they asked for remembered in `next`. It resolves otherwise, with
// who is signed in already in the cache.
export async function requireSession({ location }: { location: { href: string } }): Promise<void> {
  const instance = await loadInstance(30_000);
  if (instance.needsSetup) throw redirect({ to: "/setup" });
  try {
    await queryClient.ensureQueryData(meQueryOptions);
  } catch (err) {
    if (err instanceof UnauthenticatedError) {
      throw redirect({
        to: "/login",
        search: { next: location.href === "/" ? undefined : location.href },
      });
    }
    throw err;
  }
}

// useSessionExpiry arms the one reaction to a session that ends while a signed-in
// page is open. A 401 on any later request means the session ended (expired, revoked
// or signed out elsewhere): go to sign-in, which brings the browser back to this page
// afterwards, and then drop everything the session cached so nothing from it survives
// for the next person (WR-15). One-shot, so a burst of failing requests navigates once.
export function useSessionExpiry(): void {
  const router = useRouter();
  useEffect(() => {
    setUnauthorizedHandler((path) => {
      setUnauthorizedHandler(null);
      setCsrfToken(null);
      void router
        .navigate({
          to: "/login",
          search: { next: path === "/" ? undefined : path, expired: true },
          replace: true,
        })
        // Clear only once the signed-in pages are gone (the same order as the
        // user menu's logout), so no observer refetches into the cleared cache.
        .then(() => queryClient.clear());
    });
    return () => setUnauthorizedHandler(null);
  }, [router]);
}
