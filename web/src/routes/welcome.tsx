// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { createFileRoute, redirect, useNavigate } from "@tanstack/react-router";

import { loadInstance } from "@/api/instance";
import { queryClient } from "@/api/query-client";
import { AuthLayout } from "@/components/auth-layout";
import { WizardStepper } from "@/components/wizard-stepper";
import { LocalHostPanel, localHostQuery } from "@/features/environments/local-host-panel";
import {
  hasPermission,
  type Me,
  meQueryOptions,
  UnauthenticatedError,
  useDocumentTitle,
} from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// Wizard step 4 (D-02): the admin decides whether Bayplan manages the Docker
// host it runs on. It is a signed-in route, shown once straight after setup,
// and it is the same panel Settings shows later (UI-SPEC interpretation 1).
function WelcomePage() {
  const t = useT();
  const navigate = useNavigate();
  useDocumentTitle(t("env.welcome.tabTitle"));
  // The wordmark, as on the earlier steps, and the stepper at step 4 of 4.
  return (
    <AuthLayout title={t("common.brand.wordmark")} wide showSignOut>
      <WizardStepper current={4} />
      <LocalHostPanel mode="wizard" onDone={() => void navigate({ to: "/", replace: true })} />
    </AuthLayout>
  );
}

export const Route = createFileRoute("/welcome")({
  beforeLoad: async ({ location }) => {
    const instance = await loadInstance();
    if (instance.needsSetup) throw redirect({ to: "/setup" });

    let me: Me;
    try {
      me = await queryClient.ensureQueryData(meQueryOptions);
    } catch (err) {
      if (err instanceof UnauthenticatedError) {
        throw redirect({ to: "/login", search: { next: location.href } });
      }
      throw err;
    }

    // Adding a host needs environments.create; for anyone else this step has
    // nothing to offer, and the server refuses /api/local-host to them.
    if (!hasPermission(me, "environments.create")) throw redirect({ to: "/" });

    // Already managed (the admin came back to this address later): the choice
    // was made. A failed lookup is not a reason to block the page; the panel
    // shows the error itself.
    const host = await queryClient
      .fetchQuery({ ...localHostQuery, staleTime: 0 })
      .catch(() => null);
    if (host?.managed) throw redirect({ to: "/" });
  },
  component: WelcomePage,
});
