// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackReviewObject } from "@/api/stacks";
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

interface OtherResourcesTableProps {
  objects: StackReviewObject[];
}

const GLYPH = { added: "+", changed: "~", removed: "−", external: "" } as const;

// OtherResourcesTable is the Other resources card of the Changes tab (STK-06, D-07, D-11): the
// networks, secrets and configs a deploy would add, change or remove, and the external ones the
// stack only names. A secret or config Bayplan names by a hash of its content is a new object
// when its content changed, and the detail says the old one goes after the deploy. Names and
// details are text. A stack with none of these draws no card.
export function OtherResourcesTable({ objects }: OtherResourcesTableProps) {
  const t = useT();
  if (objects.length === 0) return null;
  const title = t("stackReview.other.title");
  return (
    <SettingsCard title={title}>
      <Table aria-label={title}>
        <TableHeader>
          <TableRow>
            <TableHead scope="col" className="w-32">
              {t("stackReview.other.columns.change")}
            </TableHead>
            <TableHead scope="col" className="w-28">
              {t("stackReview.other.columns.kind")}
            </TableHead>
            <TableHead scope="col">{t("stackReview.other.columns.name")}</TableHead>
            <TableHead scope="col">{t("stackReview.other.columns.detail")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {objects.map((o) => {
            // The server's sentence when it sent one; the page's own words for the two cases that
            // always read the same.
            const detail =
              o.detail ||
              (o.change === "external" || o.change === "removed"
                ? t(`stackReview.other.detail.${o.change}`)
                : "—");
            return (
              <TableRow key={`${o.kind}:${o.name}`}>
                <TableCell className="align-top whitespace-nowrap">
                  {GLYPH[o.change] ? (
                    <span aria-hidden="true" className="me-1 font-mono">
                      {GLYPH[o.change]}
                    </span>
                  ) : null}
                  {t(`stackReview.other.change.${o.change}`)}
                </TableCell>
                <TableCell className="align-top">{t(`stackReview.other.kind.${o.kind}`)}</TableCell>
                <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                  {o.name}
                </TableCell>
                <TableCell className="align-top text-xs whitespace-normal text-muted-foreground">
                  {detail}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </SettingsCard>
  );
}
