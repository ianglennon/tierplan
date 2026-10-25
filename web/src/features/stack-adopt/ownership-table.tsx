// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { AdoptCheck } from "@/api/stack-adopt";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

interface OwnershipTableProps {
  name: string;
  // The check, once it has settled; absent while it runs and when it failed.
  check?: AdoptCheck;
  // True while the check runs.
  checking: boolean;
}

// OwnershipTable is the card "How the pieces are treated" of the Adopt page (UI-SPEC, D-13, D-18): the counts of the
// networks, volumes, secrets and configs of the stack, then each piece with its treatment. A network or volume that
// carries the stack's label is owned by the stack under its existing name; any other, and every existing secret and
// config, is external and Bayplan never changes it. A secret or config also says it is not stack-owned and how to
// rotate it. It is a skeleton while the check runs, and says nothing when the check failed: the check card says so.
export function OwnershipTable({ name, check, checking }: OwnershipTableProps) {
  const t = useT();
  const settled = check && !checking ? check : undefined;
  if (!checking && !settled) return null;
  const objects = settled?.objects ?? [];
  const counts = settled?.counts;
  return (
    <SettingsCard title={t("stackAdoptCheck.ownership.title")}>
      {settled && counts ? (
        <>
          <p className="text-sm">
            {[
              t("stackAdoptCheck.ownership.counts.networks", { count: counts.networks }),
              t("stackAdoptCheck.ownership.counts.volumes", { count: counts.volumes }),
              t("stackAdoptCheck.ownership.counts.secrets", { count: counts.secrets }),
              t("stackAdoptCheck.ownership.counts.configs", { count: counts.configs }),
            ].join(", ")}
          </p>
          {objects.length > 0 ? (
            <section
              aria-label={t("stackAdoptCheck.ownership.caption", { name })}
              // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
              tabIndex={0}
              className="max-h-[min(32rem,60vh)] overflow-y-auto rounded-lg ring-1 ring-foreground/10 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              <Table>
                <caption className="sr-only">
                  {t("stackAdoptCheck.ownership.caption", { name })}
                </caption>
                <TableHeader>
                  <TableRow>
                    <TableHead scope="col" className="w-24">
                      {t("stackAdoptCheck.ownership.columns.kind")}
                    </TableHead>
                    <TableHead scope="col">{t("stackAdoptCheck.ownership.columns.name")}</TableHead>
                    <TableHead scope="col">
                      {t("stackAdoptCheck.ownership.columns.treatment")}
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {objects.map((o) => (
                    <TableRow key={`${o.kind}\0${o.name}`}>
                      <TableCell className="align-top whitespace-nowrap">
                        {t(`stackAdoptCheck.ownership.kind.${o.kind}`)}
                      </TableCell>
                      <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                        {o.name}
                      </TableCell>
                      <TableCell className="align-top whitespace-normal">
                        <span className="block">
                          {t(
                            o.treatment === "owned"
                              ? "stackAdoptCheck.ownership.owned"
                              : "stackAdoptCheck.ownership.external",
                          )}
                        </span>
                        {o.kind === "secret" || o.kind === "config" ? (
                          <span className="block text-xs text-muted-foreground">
                            {t("stackAdoptCheck.ownership.notStackOwned")}
                          </span>
                        ) : null}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </section>
          ) : null}
        </>
      ) : (
        <div className="flex flex-col gap-3" aria-hidden="true">
          <Skeleton className="h-5 w-1/2" />
          <Skeleton className="h-24 w-full" />
        </div>
      )}
    </SettingsCard>
  );
}
