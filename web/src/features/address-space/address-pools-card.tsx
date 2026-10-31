// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Plus, X } from "lucide-react";
import { type FormEvent, useId, useMemo, useRef, useState } from "react";

import {
  type AddressSettings,
  type AddressSettingsError,
  addressSettingsQuery,
  conflictBy,
  type PoolEntry,
  poolRowError,
  useSaveAddressPools,
} from "@/api/address-space";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { samePools, versionToSend } from "@/features/address-space/settings-version";
import { ConflictLine, TOUCH_HEIGHT } from "@/features/address-space/small-subnets-card";
import { SettingsCard } from "@/features/environments/settings-card";
import { hostBitsOf, overlaps, parseCidr } from "@/features/networks/cidr";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// The server takes up to 16 pools.
export const MAX_POOLS = 16;
// A pool's networks are at most this small.
const MAX_SIZE = 30;

export interface PoolRow {
  base: string;
  size: string;
}

export interface RowErrors {
  base?: string;
  size?: string;
}

type Translate = (key: string, params?: Record<string, unknown>) => string;

// baseMask is the prefix length of a base that is a plain IPv4 network address (no host bits) a
// pool can be cut from, or undefined for anything else.
function baseMask(base: string): number | undefined {
  const cidr = parseCidr(base);
  if (cidr === null || cidr.family !== 4 || hostBitsOf(base) !== null) return undefined;
  return cidr.bits <= MAX_SIZE ? cidr.bits : undefined;
}

// validatePoolRows checks the rows as the server will (T-10-58): each base is an IPv4 CIDR with no
// host bits, each size runs from the base's own mask to 30, and no two pools overlap. The server is
// the enforcer; this only gives the line before anything is sent. One entry per row, empty for a
// row that is fine. An overlap is reported on the later of the two rows.
export function validatePoolRows(rows: readonly PoolRow[], t: Translate): RowErrors[] {
  return rows.map((row, i) => {
    const errors: RowErrors = {};
    const mask = baseMask(row.base);
    if (mask === undefined) {
      errors.base = t("envsettings.addressPools.baseInvalid");
    } else if (rows.slice(0, i).some((other) => overlaps(other.base, row.base))) {
      errors.base = t("envsettings.addressPools.overlap", { base: row.base.trim() });
    }
    const size = row.size.trim();
    const n = /^\d{1,3}$/.test(size) ? Number(size) : undefined;
    // With no valid base there is nothing to measure the size against: the base is fixed first.
    if (mask !== undefined && (n === undefined || n < mask || n > MAX_SIZE)) {
      errors.size = t("envsettings.addressPools.sizeInvalid", { min: mask });
    }
    return errors;
  });
}

interface KeyedRow extends PoolRow {
  key: number;
}

interface AddressPoolsCardProps {
  envId: string;
  envName: string;
  // True for someone holding networks.addressing (Env Admin) on the environment.
  canEdit: boolean;
}

interface Edit {
  rows: KeyedRow[];
  // The version of the settings the person started editing, which is the one a save sends.
  version: number;
  // The pools as saved when the editing began; a save is re-based only while they still hold.
  base: PoolEntry[];
}

type Failure =
  | { kind: "conflict"; by: string }
  | { kind: "failed" }
  | { kind: "row"; row: number; type: string };

// AddressPoolsCard is "Address pools" on a standalone host's settings page (D-07 as amended): an
// optional override. Coaming reads the pools the daemon reports, or Docker's defaults when it sets
// none; pools entered here replace them. It saves on its own, and only an Env Admin can change it.
export function AddressPoolsCard({ envId, envName, canEdit }: AddressPoolsCardProps) {
  const t = useT();
  const id = useId();
  const settings = useQuery(addressSettingsQuery(envId));
  const save = useSaveAddressPools(envId);
  const nextKey = useRef(0);
  const addButton = useRef<HTMLButtonElement>(null);
  const [edit, setEdit] = useState<Edit | null>(null);
  const [failure, setFailure] = useState<Failure | null>(null);
  // Whether Save was pressed with a row the client refused: the lines show from then on.
  const [tried, setTried] = useState(false);

  // The rows as saved, with list keys that stay the same until the settings are read again.
  const saved = useMemo<KeyedRow[]>(
    () =>
      (settings.data?.pools ?? []).map((p: PoolEntry) => ({
        key: nextKey.current++,
        base: p.base,
        size: String(p.size),
      })),
    [settings.data],
  );
  const rows: readonly KeyedRow[] = edit?.rows ?? saved;
  const errors = validatePoolRows(rows, t);

  function change(next: (rows: KeyedRow[]) => KeyedRow[]) {
    if (!settings.data || !canEdit) return;
    setEdit((prev) => ({
      version: prev?.version ?? settings.data.version,
      base: prev?.base ?? settings.data.pools ?? [],
      rows: next(prev?.rows ?? saved),
    }));
    setFailure(null);
  }

  function setField(key: number, field: keyof PoolRow, value: string) {
    change((all) => all.map((r) => (r.key === key ? { ...r, [field]: value } : r)));
  }

  function remove(key: number) {
    change((all) => all.filter((r) => r.key !== key));
    // The row that had focus is gone: Add pool is where the person carries on.
    requestAnimationFrame(() => addButton.current?.focus());
  }

  function add() {
    change((all) => [...all, { key: nextKey.current++, base: "", size: "" }]);
  }

  function reload() {
    setEdit(null);
    setFailure(null);
    setTried(false);
    void settings.refetch();
  }

  function saveDone() {
    setEdit(null);
    setFailure(null);
    setTried(false);
    toastSuccess(t("envsettings.addressPools.saved"));
  }

  function failureOf(error: AddressSettingsError): Failure {
    const row = poolRowError(error);
    return error.type === "settings_changed"
      ? { kind: "conflict", by: conflictBy(error) }
      : row
        ? { kind: "row", ...row }
        : { kind: "failed" };
  }

  // submit sends the newest version this card may (see versionToSend). A 409 settings_changed
  // reads the settings once more: if the pools are unchanged the other card's save moved the
  // version, and the save is sent again, once, with the fresh version. Otherwise it is a conflict.
  // A 422 naming a row is a row failure and is never sent again.
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!canEdit || save.isPending || !settings.data) return;
    setTried(true);
    if (errors.some((e) => e.base || e.size)) return;
    const current = settings.data;
    const base = edit?.base ?? current.pools ?? [];
    const pools = rows.map((r) => ({ base: r.base.trim(), size: Number(r.size.trim()) }));
    const sent = versionToSend(
      { version: edit?.version ?? current.version, base },
      { version: current.version, slice: current.pools ?? [] },
      samePools,
    );
    try {
      await save.mutateAsync({ pools, version: sent });
      saveDone();
    } catch (e) {
      const error = e as AddressSettingsError;
      if (error.type !== "settings_changed") {
        setFailure(failureOf(error));
        return;
      }
      const read = await settings.refetch();
      const fresh: AddressSettings | undefined = read.isError ? undefined : read.data;
      if (!fresh || fresh.version === sent) {
        setFailure({ kind: "conflict", by: conflictBy(error) });
        return;
      }
      if (!samePools(fresh.pools ?? [], base)) {
        setFailure({ kind: "conflict", by: fresh.changedBy || conflictBy(error) });
        return;
      }
      try {
        await save.mutateAsync({ pools, version: fresh.version });
        saveDone();
      } catch (again) {
        setFailure(failureOf(again as AddressSettingsError));
      }
    }
  }

  // The line a row shows: what the client refuses (once Save was pressed, or the row was edited
  // after a refusal), else what the server said about it.
  function lineFor(index: number, field: "base" | "size"): string | undefined {
    const own = tried ? errors[index]?.[field] : undefined;
    if (own) return own;
    if (failure?.kind !== "row" || failure.row !== index) return undefined;
    const row = rows[index];
    if (!row) return undefined;
    if (field === "base" && failure.type === "pool_base_invalid") {
      return t("envsettings.addressPools.baseInvalid");
    }
    if (field === "base" && failure.type === "pool_overlap") {
      return t("envsettings.addressPools.overlap", { base: row.base.trim() });
    }
    if (field === "size" && failure.type === "pool_size_invalid") {
      return t("envsettings.addressPools.sizeInvalid", { min: baseMask(row.base) ?? 0 });
    }
    return undefined;
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
          {t("envsettings.addressPools.saving")}
        </>
      ) : (
        t("envsettings.addressPools.save")
      )}
    </Button>
  );

  const addPool = canEdit ? (
    <Button
      ref={addButton}
      type="button"
      variant="outline"
      className={`${TOUCH_HEIGHT} max-md:w-full`}
      aria-disabled={rows.length >= MAX_POOLS || undefined}
      onClick={() => {
        if (rows.length < MAX_POOLS) add();
      }}
    >
      <Plus aria-hidden="true" />
      {t("envsettings.addressPools.add")}
    </Button>
  ) : null;

  return (
    <SettingsCard id="address-pools" title={t("envsettings.sections.addressPools")}>
      <p className="text-sm text-muted-foreground">
        {t("envsettings.addressPools.description")}{" "}
        <Link
          to="/env/$envId/address-space/guide"
          params={{ envId }}
          search={{ part: "standalone" }}
          className="rounded-sm text-accent-ink underline underline-offset-4"
        >
          {t("poolGuide.header.generate")}
        </Link>
      </p>
      {settings.isPending ? (
        <div className="flex flex-col gap-3" aria-busy="true">
          <Skeleton className="h-8 w-full max-w-md" />
          <Skeleton className="h-8 w-40" />
        </div>
      ) : settings.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {t("envsettings.addressPools.loadFailed")}
        </p>
      ) : (
        <form onSubmit={submit} noValidate className="flex flex-col gap-4">
          {rows.length === 0 ? (
            <div className="flex flex-col gap-1">
              <p className="text-sm font-semibold">{t("envsettings.addressPools.emptyTitle")}</p>
              <p className="text-sm text-muted-foreground">
                {t("envsettings.addressPools.emptyBody")}
              </p>
            </div>
          ) : (
            <>
              <Table>
                <TableCaption className="sr-only">
                  {t("envsettings.addressPools.tableLabel")}
                </TableCaption>
                <TableHeader>
                  <TableRow>
                    <TableHead scope="col" className="min-w-56">
                      {t("envsettings.addressPools.base")}
                    </TableHead>
                    <TableHead scope="col" className="w-24">
                      {t("envsettings.addressPools.size")}
                    </TableHead>
                    {canEdit ? (
                      <TableHead scope="col" className="w-11">
                        <span className="sr-only">{t("envsettings.addressPools.actions")}</span>
                      </TableHead>
                    ) : null}
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((row, i) => {
                    const n = i + 1;
                    const baseLine = lineFor(i, "base");
                    const sizeLine = lineFor(i, "size");
                    return (
                      <TableRow key={row.key} className="align-top">
                        <TableCell>
                          <Input
                            aria-label={t("envsettings.addressPools.baseAria", { n })}
                            placeholder={t("envsettings.addressPools.basePlaceholder")}
                            autoComplete="off"
                            spellCheck={false}
                            readOnly={!canEdit}
                            value={row.base}
                            aria-invalid={baseLine ? true : undefined}
                            aria-describedby={baseLine ? `${id}-base-${row.key}` : undefined}
                            className={`${TOUCH_HEIGHT} font-mono`}
                            onChange={(e) => setField(row.key, "base", e.target.value)}
                          />
                          {baseLine ? (
                            <p
                              id={`${id}-base-${row.key}`}
                              role="alert"
                              className="mt-1 text-xs text-destructive"
                            >
                              {baseLine}
                            </p>
                          ) : null}
                        </TableCell>
                        <TableCell>
                          <Input
                            type="number"
                            inputMode="numeric"
                            aria-label={t("envsettings.addressPools.sizeAria", { n })}
                            placeholder={t("envsettings.addressPools.sizePlaceholder")}
                            readOnly={!canEdit}
                            value={row.size}
                            aria-invalid={sizeLine ? true : undefined}
                            aria-describedby={sizeLine ? `${id}-size-${row.key}` : undefined}
                            className={TOUCH_HEIGHT}
                            onChange={(e) => setField(row.key, "size", e.target.value)}
                          />
                          {sizeLine ? (
                            <p
                              id={`${id}-size-${row.key}`}
                              role="alert"
                              className="mt-1 text-xs text-destructive"
                            >
                              {sizeLine}
                            </p>
                          ) : null}
                        </TableCell>
                        {canEdit ? (
                          <TableCell>
                            <Button
                              type="button"
                              variant="ghost"
                              size="icon"
                              className="size-11 md:size-8"
                              aria-label={t("envsettings.addressPools.remove", { n })}
                              onClick={() => remove(row.key)}
                            >
                              <X aria-hidden="true" />
                            </Button>
                          </TableCell>
                        ) : null}
                      </TableRow>
                    );
                  })}
                </TableBody>
              </Table>
              <p className="text-xs text-muted-foreground">
                {t("envsettings.addressPools.sizeHelper")}
              </p>
            </>
          )}
          {failure?.kind === "conflict" ? (
            <ConflictLine by={failure.by} onReload={reload} />
          ) : failure?.kind === "failed" ? (
            <p role="alert" className="text-sm text-destructive">
              {t("envsettings.addressPools.saveFailed")} {t("envsettings.addressPools.retry")}
            </p>
          ) : null}
          <div className="flex flex-col gap-2 md:flex-row">
            {addPool}
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
