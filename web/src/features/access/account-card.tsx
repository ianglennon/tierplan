// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { enableUser, type UserDetail } from "@/api/users";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { DisableUserDialog } from "@/features/access/disable-user-dialog";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface AccountCardProps {
  user: UserDetail;
  // Called with the user as the server now has them, or with nothing when the
  // screen should simply read them again.
  onChanged: (user?: UserDetail) => void;
}

// AccountCard is the last card of the user page: the actions that change whether
// the account works at all (UI-SPEC "User page"). Here: Disable user or Enable
// user.
export function AccountCard({ user, onChanged }: AccountCardProps) {
  const t = useT();
  const [disabling, setDisabling] = useState(false);
  const [enabling, setEnabling] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const isDisabled = user.status === "disabled";

  async function enable() {
    if (inFlight.current) return;
    inFlight.current = true;
    setEnabling(true);
    setError(null);
    try {
      const result = await enableUser(user.id);
      if (result.kind === "ok") {
        toastSuccess(t("userpage.enabled", { username: user.username }));
        onChanged(result.user);
        return;
      }
      setError(
        result.kind === "not_found"
          ? t("userpage.errors.gone", { username: user.username })
          : t("userpage.enableFailed", { username: user.username }),
      );
      onChanged();
    } finally {
      inFlight.current = false;
      setEnabling(false);
    }
  }

  return (
    <SettingsCard title={t("userpage.sections.account")} id="account">
      {error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      <div className="flex flex-wrap items-center gap-3">
        {isDisabled ? (
          <Button
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            disabled={enabling}
            onClick={() => void enable()}
          >
            {enabling ? (
              <>
                <Spinner aria-hidden="true" />
                {t("userpage.account.enablePending")}
              </>
            ) : (
              t("userpage.account.enable")
            )}
          </Button>
        ) : (
          <Button
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            onClick={() => setDisabling(true)}
          >
            {t("userpage.account.disable")}
          </Button>
        )}
      </div>
      <DisableUserDialog
        user={disabling ? user : null}
        onClose={() => setDisabling(false)}
        onDone={(updated) => {
          setDisabling(false);
          onChanged(updated);
        }}
        onStale={() => onChanged()}
      />
    </SettingsCard>
  );
}
