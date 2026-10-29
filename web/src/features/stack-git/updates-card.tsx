// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { RefreshCw } from "lucide-react";
import { useId, useState } from "react";

import {
  type StackGit,
  StackGitUpdatesError,
  useCheckStackGit,
  useUpdateStackGitUpdates,
} from "@/api/stack-git";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldError } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// The range the server enforces again (D-14).
const MIN_MINUTES = 1;
const MAX_MINUTES = 1440;

// Minutes as typed: a whole number in the range, or null. A cleared field, a fraction, a negative number and a number
// outside the range are all the same refusal.
function parseMinutes(text: string): number | null {
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) return null;
  const n = Number(trimmed);
  return n >= MIN_MINUTES && n <= MAX_MINUTES ? n : null;
}

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The card's box: its title is a level 3 heading in a labelled region, 16px inside under 768px and 24px from there up.
function UpdatesShell({ children }: { children: React.ReactNode }) {
  const t = useT();
  const id = useId();
  return (
    <section aria-labelledby={id}>
      <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
        <CardHeader>
          <CardTitle>
            <h3 id={id} className="text-base font-semibold">
              {t("stackGit.updates.title")}
            </h3>
          </CardTitle>
          <CardDescription>{t("stackGit.updates.body")}</CardDescription>
        </CardHeader>
        <CardContent className="flex min-w-0 flex-col gap-4">{children}</CardContent>
      </Card>
    </section>
  );
}

// UpdatesSkeleton is the card while the stack's git settings are not there yet: the same box with the controls
// off, so nothing can be pressed before it is known what they would change.
export function UpdatesSkeleton() {
  const t = useT();
  return (
    <UpdatesShell>
      <div aria-busy="true" className="flex flex-col gap-4">
        <div className="flex items-center gap-2">
          <Switch disabled aria-label={t("stackGit.updates.poll")} />
          <Skeleton className="h-4 w-40" />
        </div>
        <Skeleton className="h-8 w-40" />
        <div className="flex flex-wrap gap-2">
          <Button type="button" variant="outline" disabled className={TOUCH_HEIGHT}>
            {t("stackGit.updates.save")}
          </Button>
          <Button type="button" variant="outline" disabled className={TOUCH_HEIGHT}>
            {t("stackGit.updates.checkNow")}
          </Button>
        </div>
      </div>
    </UpdatesShell>
  );
}

interface UpdatesCardProps {
  envId: string;
  stackId: string;
  git: StackGit;
}

// UpdatesCard is the Updates card of the Git tab (UI-SPEC "Git tab", D-14): whether Bayplan checks the repository on a
// schedule and how often, saved with its own button, and Check now with the time of the last check. A stack that
// follows a fixed commit has nothing to check, so it shows a line saying so and neither control. A person who may not
// deploy sees the settings and cannot change them.
export function UpdatesCard({ envId, stackId, git }: UpdatesCardProps) {
  const fixed = git.refKind === "commit";
  return (
    <UpdatesShell>
      {fixed ? (
        <FixedCommit />
      ) : (
        // A save, or a change made elsewhere, starts the form again from what the server holds.
        <UpdatesForm
          key={`${git.pollEnabled}:${git.pollMinutes}`}
          envId={envId}
          stackId={stackId}
          git={git}
        />
      )}
    </UpdatesShell>
  );
}

function FixedCommit() {
  const t = useT();
  return <p className="text-sm text-muted-foreground">{t("stackGit.updates.fixed")}</p>;
}

function UpdatesForm({ envId, stackId, git }: UpdatesCardProps) {
  const t = useT();
  const switchId = useId();
  const minutesId = useId();
  const needs = useNeeds("stacks.deploy", envId);
  const update = useUpdateStackGitUpdates(envId, stackId);
  const check = useCheckStackGit(envId, stackId, {
    onError: () => toastError(t("stackGit.check.failed")),
  });
  const [enabled, setEnabled] = useState(git.pollEnabled);
  const [minutes, setMinutes] = useState(String(git.pollMinutes));
  // The server refused the number although the form let it through (it was changed under us): shown like a typed one.
  const [refused, setRefused] = useState(false);

  const parsed = parseMinutes(minutes);
  const invalid = parsed === null || refused;
  const dirty = parsed !== null && (enabled !== git.pollEnabled || parsed !== git.pollMinutes);
  const canChange = needs.allowed;
  // Permissions not known yet: the button is off, with no reason to give.
  const unknown = !needs.allowed && !needs.reason;
  const saving = update.isPending;
  const checking = git.checking || check.running;
  const describedBy = [`${minutesId}-helper`, invalid ? `${minutesId}-error` : null]
    .filter(Boolean)
    .join(" ");

  function save(event: React.FormEvent) {
    event.preventDefault();
    if (!canChange || saving || parsed === null || !dirty) return;
    setRefused(false);
    update.mutate(
      { pollEnabled: enabled, pollMinutes: parsed },
      {
        onSuccess: () => toastSuccess(t("stackGit.updates.saved")),
        onError: (e) => {
          if (e instanceof StackGitUpdatesError && e.problem?.type === "poll_interval") {
            setRefused(true);
            return;
          }
          toastError(t("stackGit.updates.saveFailed"));
        },
      },
    );
  }

  const saveButton = (
    <Button
      type="submit"
      variant="outline"
      className={TOUCH_HEIGHT}
      disabled={unknown || (canChange && (!dirty || saving))}
    >
      {saving ? (
        <>
          <Spinner aria-hidden="true" />
          {t("stackGit.updates.saving")}
        </>
      ) : (
        t("stackGit.updates.save")
      )}
    </Button>
  );

  return (
    <form noValidate onSubmit={save} className="flex flex-col gap-4">
      <fieldset disabled={saving} className="contents">
        <div className="flex items-center gap-2 max-md:min-h-11">
          <Switch
            id={switchId}
            checked={enabled}
            onCheckedChange={setEnabled}
            disabled={!canChange}
          />
          <Label htmlFor={switchId}>{t("stackGit.updates.poll")}</Label>
        </div>
        <Field data-invalid={invalid ? true : undefined}>
          <Label htmlFor={minutesId}>{t("stackGit.updates.minutes")}</Label>
          <Input
            id={minutesId}
            type="number"
            inputMode="numeric"
            min={MIN_MINUTES}
            max={MAX_MINUTES}
            step={1}
            value={minutes}
            className={`${TOUCH_HEIGHT} w-full md:w-40`}
            disabled={!canChange || !enabled}
            aria-invalid={invalid}
            aria-describedby={describedBy}
            onChange={(e) => {
              setMinutes(e.target.value);
              setRefused(false);
            }}
          />
          <FieldDescription id={`${minutesId}-helper`}>
            {t("stackGit.updates.helper")}
          </FieldDescription>
          {invalid ? (
            <FieldError id={`${minutesId}-error`}>{t("stackGit.updates.invalid")}</FieldError>
          ) : null}
        </Field>
      </fieldset>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        {canChange || unknown ? (
          saveButton
        ) : (
          <NeedsTooltip reason={needs.reason}>{saveButton}</NeedsTooltip>
        )}
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={checking}
          onClick={check.check}
        >
          <RefreshCw
            aria-hidden="true"
            className={checking ? "animate-spin motion-reduce:animate-none" : undefined}
          />
          {checking ? t("stackGit.updates.checking") : t("stackGit.updates.checkNow")}
        </Button>
        <p className="text-sm text-muted-foreground">
          {git.checkedAt ? (
            <>
              {t("stackGit.updates.lastChecked")} <RelativeTime value={git.checkedAt} />
            </>
          ) : (
            t("stackGit.updates.neverChecked")
          )}
        </p>
      </div>
    </form>
  );
}
