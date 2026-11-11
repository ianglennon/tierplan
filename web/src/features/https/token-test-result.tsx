// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, Circle, CircleX, Info, Minus, TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";

import type { ACMENameTest, ACMETokenTest } from "@/api/https";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { errorCopy } from "@/features/https/copy";
import { rich } from "@/features/https/sentence";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";
import { cn } from "@/lib/utils";

interface TokenTestResultProps {
  result: ACMETokenTest;
  // True when the result came from a save that stored nothing (D-07): the line says so.
  notSaved?: boolean;
  // True when a field the test used has changed since: the result is greyed and no longer announced.
  stale?: boolean;
}

type RowState = "done" | "failed" | "skipped" | "waiting";

const ROW_IDS = ["zone", "created", "read", "deleted"] as const;

type Translate = ReturnType<typeof useT>;

function RowIcon({ state }: { state: RowState }) {
  switch (state) {
    case "done":
      return <Check aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />;
    case "failed":
      return <CircleX aria-hidden="true" className="size-4 shrink-0 text-destructive" />;
    case "skipped":
      return <Minus aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />;
    default:
      return <Circle aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />;
  }
}

interface RowProps {
  icon: ReactNode;
  // The state in words for a screen reader, after the row's text.
  state: string;
  children: ReactNode;
  // A line under the row, in small muted text.
  extra?: ReactNode;
}

function Row({ icon, state, children, extra }: RowProps) {
  return (
    <li className="flex min-w-0 items-start gap-2 text-sm">
      <span className="mt-0.5">{icon}</span>
      <span className="flex min-w-0 flex-col gap-0.5">
        <span className="min-w-0 break-words">
          {children} <span className="sr-only">({state})</span>
        </span>
        {extra ? (
          <span className="min-w-0 break-words text-xs text-muted-foreground">{extra}</span>
        ) : null}
      </span>
    </li>
  );
}

function resolverText(t: Translate, resolvers: readonly string[] | null | undefined): string {
  return resolvers?.length ? resolvers.join(", ") : t("https.error.resolversDefault");
}

// InfoRow is the non-blocking row (D-09): what the resolvers showed. It never changes the verdict.
function InfoRow({ info }: { info: ACMENameTest["info"] }) {
  const t = useT();
  const note = t("https.dns.result.state.note");
  const resolvers = resolverText(t, info.resolvers);
  const values = { resolvers, seconds: String(info.seconds) };
  switch (info.state) {
    case "seen":
      return (
        <Row
          icon={<Info aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />}
          state={note}
        >
          {rich(t, "https.dns.result.info.seen", values)}
        </Row>
      );
    case "not_seen":
      return (
        <Row
          icon={
            <TriangleAlert
              aria-hidden="true"
              className="size-4 shrink-0 text-status-warning-text"
            />
          }
          state={note}
          extra={rich(t, "https.dns.result.info.notSeenHint", values)}
        >
          {rich(t, "https.dns.result.info.notSeen", values)}
        </Row>
      );
    default:
      return (
        <Row
          icon={<Minus aria-hidden="true" className="size-4 shrink-0 text-muted-foreground" />}
          state={note}
        >
          {t("https.dns.result.info.notChecked")}
        </Row>
      );
  }
}

// NameRows are the rows of one name, in the order the test ran them: the delegation (when the challenge
// name is a CNAME), then zone, created, read back and deleted, then the info row.
function NameRows({ name }: { name: ACMENameTest }) {
  const t = useT();
  const stateOf = new Map((name.steps ?? []).map((s) => [s.id, s.state] as const));
  const word = (state: RowState) => t(`https.dns.result.state.${state}`);
  const target = name.delegatedTo ?? "";

  const label = (id: (typeof ROW_IDS)[number], state: RowState): ReactNode => {
    switch (id) {
      case "zone":
        return state === "failed"
          ? t("https.dns.result.row.zoneFailed")
          : rich(t, "https.dns.result.row.zoneFound", { zone: name.zone });
      case "created":
        return rich(t, "https.dns.result.row.created", { record: name.record });
      case "read":
        return t("https.dns.result.row.read");
      case "deleted":
        return state === "failed"
          ? t("https.dns.result.row.deleteFailed")
          : t("https.dns.result.row.deleted");
    }
  };

  return (
    <ol className="flex min-w-0 flex-col gap-2">
      {target === "" ? null : (
        <Row icon={<RowIcon state="done" />} state={word("done")}>
          {name.ownZone
            ? rich(t, "https.dns.result.row.delegatedOwnZone", { zone: name.ownZone, target })
            : rich(t, "https.dns.result.row.delegated", { target })}
        </Row>
      )}
      {ROW_IDS.map((id) => {
        const state: RowState = stateOf.get(id) ?? "waiting";
        return (
          <Row
            key={id}
            icon={<RowIcon state={state} />}
            state={word(state)}
            extra={
              id === "deleted" && state === "failed"
                ? t("https.dns.result.row.deleteKept")
                : undefined
            }
          >
            {label(id, state)}
          </Row>
        );
      })}
      <InfoRow info={name.info} />
    </ol>
  );
}

// NameError is the classified error of a failing name: the headline and what to fix, from the same
// copy as the last-run error panel. It is a note, so the status region announces the result once.
function NameError({ name }: { name: ACMENameTest }) {
  const t = useT();
  const error = name.error;
  if (!error) return null;
  const { headline, fix } = errorCopy(t, error.code, {
    host: error.host || name.name,
    zone: error.zone ?? name.zone,
    target: error.target ?? name.delegatedTo ?? "",
    resolvers: resolverText(t, error.resolvers),
    time: formatClock(error.retryAt),
  });
  return (
    <Alert variant="destructive" role="note" className="min-w-0">
      <CircleX aria-hidden="true" />
      <AlertTitle className="min-w-0 break-words">{headline}</AlertTitle>
      <AlertDescription className="min-w-0 break-words">{fix}</AlertDescription>
    </Alert>
  );
}

// TokenTestResult is the result of a DNS token test (D-08, D-09, D-14): a headline, one group per
// certificate name with the rows the test ran, and the classified error of a failing name. Two names
// that fail with the same class show the error once, under the last of them.
export function TokenTestResult({ result, notSaved = false, stale = false }: TokenTestResultProps) {
  const t = useT();
  const names = result.names ?? [];
  const drawsError = (i: number) => {
    const code = names[i]?.error?.code;
    return code !== undefined && !names.slice(i + 1).some((n) => n.error?.code === code);
  };

  return (
    <div
      role="status"
      aria-live={stale ? "off" : "polite"}
      className={cn("flex min-w-0 flex-col gap-4 rounded-lg border p-4", stale && "opacity-60")}
    >
      <div className="flex flex-col gap-1">
        <p className="flex items-center gap-2 text-sm font-semibold">
          {result.passed ? (
            <Check aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />
          ) : (
            <CircleX aria-hidden="true" className="size-4 shrink-0 text-destructive" />
          )}
          <span>{result.passed ? t("https.dns.result.passed") : t("https.dns.result.failed")}</span>
        </p>
        {notSaved ? <p className="text-sm">{t("https.dns.result.notSaved")}</p> : null}
      </div>
      {names.map((name, i) => (
        <section key={name.name} className="flex min-w-0 flex-col gap-2">
          <h3 className="min-w-0 break-all font-mono text-sm font-semibold">{name.name}</h3>
          <NameRows name={name} />
          {drawsError(i) ? <NameError name={name} /> : null}
        </section>
      ))}
      {stale ? (
        <p className="text-xs text-muted-foreground" aria-live="off">
          {t("https.dns.result.stale")}
        </p>
      ) : null}
    </div>
  );
}
