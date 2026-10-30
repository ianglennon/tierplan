// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useId, useRef, useState } from "react";

import { setInstanceAdmin, type UserDetail } from "@/api/users";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface InstanceAdminCardProps {
  user: UserDetail;
  // Called with the user as the server now has them, or with nothing when the
  // screen should simply read them again.
  onChanged: (user?: UserDetail) => void;
}

// InstanceAdminCard is the one switch that gives a user instance authority
// (D-02): per user, never through a team. Turning it on or off asks first. For
// the last active instance admin the switch is shown but unavailable, with the
// reason, because the server would refuse (T-03-54); if it refuses anyway, the
// card says so and the page reads the user again.
export function InstanceAdminCard({ user, onChanged }: InstanceAdminCardProps) {
  const t = useT();
  const labelId = useId();
  const helperId = useId();
  // The state the pending dialog would set, or null while it is closed.
  const [target, setTarget] = useState<boolean | null>(null);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  async function apply() {
    if (target === null || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    setError(null);
    try {
      const result = await setInstanceAdmin(user.id, target);
      if (result.kind === "ok") {
        setTarget(null);
        onChanged(result.user);
        return;
      }
      if (result.kind === "last_instance_admin") setError(t("userpage.errors.lastAdmin"));
      else if (result.kind === "not_found")
        setError(t("userpage.errors.gone", { username: user.username }));
      else setError(t("userpage.instanceAdmin.failed", { username: user.username }));
      onChanged();
    } finally {
      inFlight.current = false;
      setPending(false);
    }
  }

  function change(open: boolean) {
    if (open || pending) return;
    setTarget(null);
  }

  const turningOn = target === true;
  const copy = turningOn ? "userpage.instanceAdmin.on" : "userpage.instanceAdmin.off";

  return (
    <SettingsCard title={t("userpage.sections.instanceAdmin")} id="instance-admin">
      {error && target === null ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      <div className="flex items-start gap-3">
        <NeedsTooltip reason={user.lastInstanceAdmin ? t("needs.lastAdmin") : undefined}>
          <Switch
            aria-labelledby={labelId}
            aria-describedby={helperId}
            checked={user.instanceAdmin}
            onCheckedChange={(next) => {
              setError(null);
              setTarget(next);
            }}
            className="mt-0.5"
          />
        </NeedsTooltip>
        <div className="flex min-w-0 flex-col gap-1">
          <Label id={labelId} className="text-sm font-medium">
            {t("userpage.instanceAdmin.label")}
          </Label>
          <p id={helperId} className="text-sm text-muted-foreground">
            {t("userpage.instanceAdmin.helper")}
          </p>
        </div>
      </div>
      <AlertDialog open={target !== null} onOpenChange={change}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle className="break-words">
              {t(`${copy}.title`, { username: user.username })}
            </AlertDialogTitle>
            <AlertDialogDescription className="break-words">
              {t(`${copy}.body`)}
            </AlertDialogDescription>
          </AlertDialogHeader>
          {error ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          ) : null}
          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
              {t("userpage.instanceAdmin.cancel")}
            </AlertDialogCancel>
            <Button
              type="button"
              disabled={pending}
              className={TOUCH_HEIGHT}
              onClick={() => void apply()}
            >
              {pending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t(`${copy}.pending`)}
                </>
              ) : (
                t(`${copy}.confirm`)
              )}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsCard>
  );
}
