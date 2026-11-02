// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

import type { StackRevisionDetail } from "@/api/stack-history";
import { MaskedValue } from "@/components/masked-value";
import { Badge } from "@/components/ui/badge";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { TruncatedText } from "@/features/resources/resource-table";
import { RevisionSource } from "@/features/stack-history/revision-source";
import { DigestChip } from "@/features/stacks/digest-chip";
import { useT } from "@/i18n/use-t";

interface RevisionContentsProps {
  detail: StackRevisionDetail;
  // The stack's name: a swarm names its networks name_network, and the list shows the name the file gives.
  stackName?: string;
}

// A titled block with its count; the table inside it scrolls sideways on a narrow screen.
function Section({
  title,
  count,
  children,
}: {
  title: string;
  count: string;
  children: ReactNode;
}) {
  return (
    <section className="flex min-w-0 flex-col gap-2">
      <div className="flex flex-wrap items-baseline gap-2">
        <h4 className="text-sm font-medium">{title}</h4>
        <span className="text-sm text-muted-foreground">{count}</span>
      </div>
      <div className="rounded-md border">{children}</div>
    </section>
  );
}

// RevisionContents is exactly what a roll back to the revision would restore (STK-08, D-05): its files, its
// variables and the images it pinned. The server withholds a secret variable's value and a secret file's
// content, so a secret is drawn as hidden and there is no way to reveal it from History (P6 D-39).
export function RevisionContents({ detail, stackName }: RevisionContentsProps) {
  const t = useT();
  const seq = detail.item.seq;
  const files = detail.files ?? [];
  const vars = detail.vars ?? [];
  const pins = detail.pins ?? [];
  // The subnets the revision's deploy gave its networks (D-20), by network name. A rollback re-uses the ones
  // it copied, so its rows say so. A revision that recorded none has no section.
  const prefix = stackName ? `${stackName}_` : "";
  const subnets = Object.entries(detail.subnets ?? {})
    .map(([name, subnet]) => ({
      name:
        prefix && name.startsWith(prefix) && name.length > prefix.length
          ? name.slice(prefix.length)
          : name,
      subnet,
    }))
    .sort((a, b) => a.name.localeCompare(b.name));
  const reused = detail.item.origin === "rollback";
  return (
    <div className="flex min-w-0 flex-col gap-6">
      <p className="text-sm text-muted-foreground">{t("stackRevision.contents.helper")}</p>

      {detail.item.commit ? (
        <section className="flex min-w-0 flex-col gap-2">
          <h4 className="text-sm font-medium">{t("stackRevision.contents.source.title")}</h4>
          <RevisionSource item={detail.item} />
        </section>
      ) : null}

      <Section
        title={t("stackRevision.contents.files.title")}
        count={t("stackRevision.contents.files.count", { count: files.length })}
      >
        <Table>
          <TableCaption className="sr-only">
            {t("stackRevision.contents.files.caption", { seq })}
          </TableCaption>
          <TableHeader>
            <TableRow>
              <TableHead scope="col">{t("stackRevision.contents.files.file")}</TableHead>
              <TableHead scope="col">{t("stackRevision.contents.files.type")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {files.map((file) => (
              <TableRow key={file.path}>
                <TableCell>
                  <TruncatedText mono>{file.path}</TruncatedText>
                </TableCell>
                <TableCell>
                  {file.secret ? (
                    <span className="flex flex-wrap items-center gap-2">
                      <Badge variant="outline">{t("stackRevision.contents.files.secret")}</Badge>
                      <span className="text-muted-foreground">
                        {t("stackRevision.contents.files.hidden")}
                      </span>
                    </span>
                  ) : (
                    t("stackRevision.contents.files.plain")
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Section>

      <Section
        title={t("stackRevision.contents.vars.title")}
        count={t("stackRevision.contents.vars.count", { count: vars.length })}
      >
        {vars.length === 0 ? (
          <p className="p-3 text-sm text-muted-foreground">
            {t("stackRevision.contents.vars.none")}
          </p>
        ) : (
          <Table>
            <TableCaption className="sr-only">
              {t("stackRevision.contents.vars.caption", { seq })}
            </TableCaption>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">{t("stackRevision.contents.vars.name")}</TableHead>
                <TableHead scope="col">{t("stackRevision.contents.vars.value")}</TableHead>
                <TableHead scope="col">{t("stackRevision.contents.vars.secret")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {vars.map((v) => (
                <TableRow key={v.name}>
                  <TableCell>
                    <TruncatedText mono>{v.name}</TruncatedText>
                  </TableCell>
                  <TableCell className="whitespace-normal">
                    {v.secret ? (
                      <MaskedValue name={v.name} />
                    ) : (
                      <span className="font-mono text-sm break-all">{v.value}</span>
                    )}
                  </TableCell>
                  <TableCell>
                    {v.secret
                      ? t("stackRevision.contents.vars.yes")
                      : t("stackRevision.contents.vars.no")}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>

      {subnets.length > 0 ? (
        <Section
          title={t("stackHistory.network.title")}
          count={t("stackHistory.network.count", { count: subnets.length })}
        >
          <ul className="flex flex-col divide-y text-sm">
            {subnets.map((n) => (
              <li key={n.name} className="flex flex-wrap items-baseline gap-x-2 px-3 py-2">
                <span className="break-all">
                  {t("stackHistory.network.line", { name: n.name })}{" "}
                  <span className="font-mono">{n.subnet}</span>
                </span>
                {reused ? (
                  <span className="text-xs text-muted-foreground">
                    {t("stackHistory.network.reused")}
                  </span>
                ) : null}
              </li>
            ))}
          </ul>
        </Section>
      ) : null}

      <Section
        title={t("stackRevision.contents.pins.title")}
        count={t("stackRevision.contents.pins.count", { count: pins.length })}
      >
        {pins.length === 0 ? (
          <p className="p-3 text-sm text-muted-foreground">
            {t("stackRevision.contents.pins.none")}
          </p>
        ) : (
          <Table>
            <TableCaption className="sr-only">
              {t("stackRevision.contents.pins.caption", { seq })}
            </TableCaption>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">{t("stackRevision.contents.pins.service")}</TableHead>
                <TableHead scope="col">{t("stackRevision.contents.pins.image")}</TableHead>
                <TableHead scope="col">{t("stackRevision.contents.pins.digest")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {pins.map((pin) => (
                <TableRow key={pin.service}>
                  <TableCell>
                    <TruncatedText mono>{pin.service}</TruncatedText>
                  </TableCell>
                  <TableCell className="font-mono text-xs break-all whitespace-normal">
                    {pin.image}
                  </TableCell>
                  <TableCell>
                    {pin.digest ? (
                      <DigestChip digest={pin.digest} />
                    ) : (
                      <span className="text-muted-foreground">{t("stackHistory.missing")}</span>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>
    </div>
  );
}
