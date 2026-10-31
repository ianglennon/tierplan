// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { type FormEvent, useId, useState } from "react";

import {
  type AddressPool,
  addressSettingsQuery,
  addressSpaceQuery,
  conflictBy,
  useSaveSmallSubnets,
} from "@/api/address-space";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { SettingsCard } from "@/features/environments/settings-card";
import { usableOf } from "@/features/subnet-picker/subnet-picker";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
export const TOUCH_HEIGHT = "h-11 md:h-8";

// The sizes a stack network may be given: the server takes 25 to 28.
export const SMALL_SUBNET_MASKS = [25, 26, 27, 28] as const;

// roomFor is about how many more networks of the mask the first pool can still give, or undefined
// when that cannot be said: no pool, a pool that was not measured, or a mask larger than the
// pool's own networks (a /24 pool cannot give /22 blocks). Each free block of the pool is cut into
// 2^(mask - pool mask) blocks of the mask.
export function roomFor(pools: readonly AddressPool[] | null | undefined, mask: number) {
  const pool = pools?.[0];
  if (!pool || pool.state === "unknown" || mask < pool.mask) return undefined;
  const free = Math.max(0, pool.capacity - pool.used);
  return free * 2 ** (mask - pool.mask);
}

interface ConflictLineProps {
  // Who saved last; empty when the server did not know the person.
  by: string;
  onReload: () => void;
}

// ConflictLine is the answer to a save made against settings that someone else saved since they
// were read (409 settings_changed). It names the saver and offers Reload.
export function ConflictLine({ by, onReload }: ConflictLineProps) {
  const t = useT();
  return (
    <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
      <p className="text-destructive">
        {by
          ? t("envsettings.settingsConflict.message", { user: by })
          : t("envsettings.settingsConflict.messageAnon")}
      </p>
      <Button
        type="button"
        variant="outline"
        className={`${TOUCH_HEIGHT} max-md:w-full`}
        onClick={onReload}
      >
        {t("envsettings.settingsConflict.reload")}
      </Button>
    </div>
  );
}

interface SmallSubnetsCardProps {
  envId: string;
  envName: string;
  // True for someone holding networks.addressing (Env Admin) on the environment.
  canEdit: boolean;
}

interface Edit {
  enabled: boolean;
  mask: number;
  // The version of the settings the person started editing, which is the one a save sends.
  version: number;
}

type Failure = { kind: "conflict"; by: string } | { kind: "failed" };

// SmallSubnetsCard is "Default stack subnets" on an environment's settings page (NET-06, D-21): the
// opt-in to give the networks a stack creates for the first time a smaller subnet than Docker's
// default, and how small. It saves on its own. Below Env Admin the fields are read-only and Save
// says what is needed.
export function SmallSubnetsCard({ envId, envName, canEdit }: SmallSubnetsCardProps) {
  const t = useT();
  const id = useId();
  const settings = useQuery(addressSettingsQuery(envId));
  const space = useQuery({ ...addressSpaceQuery(envId), refetchInterval: false });
  const save = useSaveSmallSubnets(envId);
  // What was changed, until the person saves: a field not touched shows the environment's value.
  const [edit, setEdit] = useState<Edit | null>(null);
  const [failure, setFailure] = useState<Failure | null>(null);

  const saved = settings.data?.smallSubnets;
  const enabled = edit?.enabled ?? saved?.enabled ?? false;
  const mask = edit?.mask ?? saved?.mask ?? 26;
  const room = roomFor(space.data?.pools, mask);

  function change(patch: Partial<Pick<Edit, "enabled" | "mask">>) {
    if (!saved || !settings.data || !canEdit) return;
    setEdit((prev) => ({
      enabled: saved.enabled,
      mask: saved.mask,
      version: settings.data.version,
      ...prev,
      ...patch,
    }));
    setFailure(null);
  }

  function reload() {
    setEdit(null);
    setFailure(null);
    void settings.refetch();
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!canEdit || save.isPending || !settings.data) return;
    save.mutate(
      { enabled, mask, version: edit?.version ?? settings.data.version },
      {
        onSuccess: () => {
          setEdit(null);
          setFailure(null);
          toastSuccess(t("envsettings.smallSubnets.saved"));
        },
        onError: (error) => {
          setFailure(
            error.type === "settings_changed"
              ? { kind: "conflict", by: conflictBy(error) }
              : { kind: "failed" },
          );
        },
      },
    );
  }

  const saveButton = (
    <Button
      type="submit"
      variant="outline"
      className={`${TOUCH_HEIGHT} max-md:w-full`}
      aria-disabled={save.isPending || undefined}
    >
      {save.isPending ? (
        <>
          <Spinner aria-hidden="true" />
          {t("envsettings.smallSubnets.saving")}
        </>
      ) : (
        t("envsettings.smallSubnets.save")
      )}
    </Button>
  );

  return (
    <SettingsCard id="default-subnets" title={t("envsettings.sections.smallSubnets")}>
      <p className="text-sm text-muted-foreground">{t("envsettings.smallSubnets.description")}</p>
      {settings.isPending ? (
        <div className="flex flex-col gap-3" aria-busy="true">
          <Skeleton className="h-8 w-64" />
          <Skeleton className="h-8 w-40" />
        </div>
      ) : settings.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {t("envsettings.smallSubnets.loadFailed")}
        </p>
      ) : (
        <form onSubmit={submit} noValidate className="flex flex-col gap-4">
          <div className="flex items-center gap-3">
            <Switch
              id={`${id}-on`}
              checked={enabled}
              disabled={!canEdit || save.isPending}
              onCheckedChange={(checked) => change({ enabled: checked })}
            />
            <Label htmlFor={`${id}-on`} className="text-sm">
              {t("envsettings.smallSubnets.switch")}
            </Label>
          </div>
          <Field className="md:max-w-64">
            <FieldLabel htmlFor={`${id}-size`}>{t("envsettings.smallSubnets.size")}</FieldLabel>
            <Select
              value={String(mask)}
              disabled={!enabled || !canEdit || save.isPending}
              onValueChange={(v) => change({ mask: Number(v) })}
            >
              <SelectTrigger
                id={`${id}-size`}
                className="w-full max-md:h-11"
                aria-disabled={!enabled || undefined}
                aria-describedby={`${id}-capacity`}
              >
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {SMALL_SUBNET_MASKS.map((m) => (
                  <SelectItem key={m} value={String(m)}>
                    {t("envsettings.smallSubnets.sizeItem", { mask: m, usable: usableOf(m) })}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <FieldDescription id={`${id}-capacity`} aria-live="polite">
              {t("envsettings.smallSubnets.capacity", { mask, usable: usableOf(mask) })}
              {room === undefined
                ? null
                : ` ${t("envsettings.smallSubnets.room", { count: room })}`}
            </FieldDescription>
          </Field>
          {failure?.kind === "conflict" ? (
            <ConflictLine by={failure.by} onReload={reload} />
          ) : failure?.kind === "failed" ? (
            <p role="alert" className="text-sm text-destructive">
              {t("envsettings.smallSubnets.saveFailed")} {t("envsettings.smallSubnets.retry")}
            </p>
          ) : null}
          <div>
            {canEdit ? (
              saveButton
            ) : (
              <NeedsTooltip reason={t("envsettings.needsAdmin", { name: envName })}>
                {saveButton}
              </NeedsTooltip>
            )}
          </div>
        </form>
      )}
    </SettingsCard>
  );
}
