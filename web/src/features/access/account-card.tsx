// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useRef, useState } from "react";

import { enableUser, type UserDetail, unlockUser } from "@/api/users";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { DeleteUserDialog } from "@/features/access/delete-user-dialog";
import { DisableUserDialog } from "@/features/access/disable-user-dialog";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface AccountCardProps {
  user: UserDetail;
  // Called with the user as the server now has them, or with nothing when the
  // screen should simply read them again.
  onChanged: (user?: UserDetail) => void;
  // Called once the user is deleted, when there is no user page left to show.
  onDeleted: () => void;
}

// AccountCard is the last card of the user page: the actions that change whether
// the account works at all (UI-SPEC "User page"): Unlock while the account is
// locked, Disable user or Enable user, and Delete user…. For the last active
// instance admin, Disable and Delete are shown but unavailable, with the reason,
// because the server would refuse (T-03-54). Reset password joins them with its
// own API.
export function AccountCard({ user, onChanged, onDeleted }: AccountCardProps) {
  const t = useT();
  const [disabling, setDisabling] = useState(false);
  const [deleting, setDeleting] = useState(false);
  const [busy, setBusy] = useState<"enable" | "unlock" | null>(null);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const isDisabled = user.status === "disabled";
  const isLocked = user.status === "locked";
  const guard = user.lastInstanceAdmin ? t("needs.lastAdmin") : undefined;

  async function run(kind: "enable" | "unlock") {
    if (inFlight.current) return;
    inFlight.current = true;
    setBusy(kind);
    setError(null);
    try {
      const result = await (kind === "enable" ? enableUser(user.id) : unlockUser(user.id));
      if (result.kind === "ok") {
        toastSuccess(
          t(kind === "enable" ? "userpage.enabled" : "userpage.unlocked", {
            username: user.username,
          }),
        );
        onChanged(result.user);
        return;
      }
      setError(
        result.kind === "not_found"
          ? t("userpage.errors.gone", { username: user.username })
          : t(kind === "enable" ? "userpage.enableFailed" : "userpage.unlockFailed", {
              username: user.username,
            }),
      );
      onChanged();
    } finally {
      inFlight.current = false;
      setBusy(null);
    }
  }

  return (
    <SettingsCard title={t("userpage.sections.account")} id="account">
      {error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      {isLocked ? (
        <div className="flex flex-wrap items-center gap-3">
          <p className="text-sm text-muted-foreground">
            {t("userpage.account.lockedUntil", { time: formatAbsolute(user.lockedUntil) })}
          </p>
          <Button
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            disabled={busy === "unlock"}
            onClick={() => void run("unlock")}
          >
            {busy === "unlock" ? (
              <>
                <Spinner aria-hidden="true" />
                {t("userpage.account.unlockPending")}
              </>
            ) : (
              t("userpage.account.unlock")
            )}
          </Button>
        </div>
      ) : null}
      <div className="flex flex-wrap items-center gap-3">
        {isDisabled ? (
          <Button
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            disabled={busy === "enable"}
            onClick={() => void run("enable")}
          >
            {busy === "enable" ? (
              <>
                <Spinner aria-hidden="true" />
                {t("userpage.account.enablePending")}
              </>
            ) : (
              t("userpage.account.enable")
            )}
          </Button>
        ) : (
          <NeedsTooltip reason={guard}>
            <Button
              type="button"
              variant="outline"
              className={`${TOUCH_HEIGHT} max-md:w-full`}
              onClick={() => setDisabling(true)}
            >
              {t("userpage.account.disable")}
            </Button>
          </NeedsTooltip>
        )}
        <NeedsTooltip reason={guard}>
          <Button
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} border-destructive/40 text-destructive hover:text-destructive max-md:w-full`}
            onClick={() => setDeleting(true)}
          >
            {t("userpage.account.delete")}
          </Button>
        </NeedsTooltip>
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
      <DeleteUserDialog
        user={deleting ? user : null}
        onClose={() => setDeleting(false)}
        onDone={() => {
          setDeleting(false);
          onDeleted();
        }}
        onStale={() => onChanged()}
      />
    </SettingsCard>
  );
}
