// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Bell, TriangleAlert } from "lucide-react";
import { type ReactNode, useRef, useState } from "react";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { ENV_WARNINGS_KEY, WARNINGS_KEY } from "@/api/warnings";
import { isPlainHttpRemote } from "@/components/auth-layout";
import { useNow } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemTitle,
} from "@/components/ui/item";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@/components/ui/popover";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import { InstallDialog } from "@/features/environments/install-dialog";
import { ReasonText } from "@/features/environments/reason-text";
import { hasPermission, useAnyEnvironmentPermission, useMe, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { formatClock, formatRelative } from "@/lib/format";
import { toastError } from "@/lib/toast";

// Build-time source URL (Vite define). The plain-HTTP fix link points at the
// project's own pages, which say how to serve Bayplan over HTTPS.
declare const __BAYPLAN_SOURCE_URL__: string;

export type Warning = components["schemas"]["WarningBody"];

// The reverse proxy warnings (D-53) and where their copy lives in the bell
// namespace. The page they point to ships with the source, so the link is built
// from the same source URL the footer uses.
const PROXY_COPY = {
  proxy_untrusted_forwarded: "untrustedForwarded",
  proxy_url_mismatch: "urlMismatch",
  proxy_websocket_failing: "webSocketFailing",
} as const;
const PROXY_DOCS_PATH = "/blob/main/docs/reverse-proxy.md";

function proxyCopy(id: Warning["id"]): (typeof PROXY_COPY)[keyof typeof PROXY_COPY] | undefined {
  return id in PROXY_COPY ? PROXY_COPY[id as keyof typeof PROXY_COPY] : undefined;
}
const REFETCH_MS = 60_000;
// The environment problems are polled this often whatever the stream says: the
// health stream carries no frame for an update that failed or a certificate that
// ran short, and it may be down (D-36).
const ENV_REFETCH_MS = 30_000;
// More offline environments than this collapse into one item, and the badge
// shows this many at most before it reads "9+" (UI-SPEC UI Considerations).
const COLLAPSE_OFFLINE_ABOVE = 5;
const BADGE_CAP = 9;

export type EnvWarning = components["schemas"]["EnvWarningBody"];

// useWarnings reads the instance warnings the server knows about: the data
// directory is not persisted, and the generated encryption key has not been
// backed up. Only an admin may ask (settings.read), so for anyone else the
// query never runs and the list stays empty (D-59). The plain-HTTP warning is
// not here: only the browser knows how it reached the server. loading is true
// until the first answer, so the bell never claims there is nothing before it
// knows.
export function useWarnings(): { items: Warning[]; allowed: boolean; loading: boolean } {
  const allowed = usePermission("settings.read");
  const { data, isPending } = useQuery({
    queryKey: WARNINGS_KEY,
    enabled: allowed,
    refetchInterval: REFETCH_MS,
    queryFn: async () => {
      const { data: body } = await api.GET("/api/warnings");
      if (!body) throw new Error("GET /api/warnings failed");
      return body.items ?? [];
    },
  });
  return { items: allowed ? (data ?? []) : [], allowed, loading: allowed && isPending };
}

// useEnvWarnings reads the environment problems (D-37, D-39): offline
// environments, certificates, clocks, updates, long-pending environments, re-initialised
// swarms and the fold offer. The server filters them to the environments the caller may
// read, so the query runs for anyone who may read one. A refresh that fails keeps the
// last items, and the list is read again every 30 seconds and whenever the health
// stream reports a change (it invalidates this key).
export function useEnvWarnings(): { items: EnvWarning[]; allowed: boolean; loading: boolean } {
  const allowed = useAnyEnvironmentPermission("environments.read");
  const { data, isPending } = useQuery({
    queryKey: ENV_WARNINGS_KEY,
    enabled: allowed,
    refetchInterval: ENV_REFETCH_MS,
    queryFn: async () => {
      const { data: body } = await api.GET("/api/environments/warnings");
      if (!body) throw new Error("GET /api/environments/warnings failed");
      return body.items ?? [];
    },
  });
  return { items: allowed ? (data ?? []) : [], allowed, loading: allowed && isPending };
}

// One line of the bell. Environment items, the collapsed offline item and the
// instance items all have a severity and, where it is known, when they began.
type Entry =
  | { kind: "plainHttp"; key: string; severity: "warning"; since?: string }
  | { kind: "keyBackup"; key: string; severity: "warning"; since?: string }
  | { kind: "proxy"; key: string; severity: "warning"; since?: string; w: Warning }
  | { kind: "env"; key: string; severity: EnvWarning["severity"]; since?: string; w: EnvWarning }
  | { kind: "manyOffline"; key: string; severity: "error"; since?: string; count: number };

const entryKey = (w: EnvWarning) => `${w.id}:${w.envId ?? ""}:${w.agentId ?? w.node ?? ""}`;

// bellEntries is what the bell lists, in order: errors before warnings, then newest
// first, and what has no time after what has, in the order it came. More than five
// offline environments are one item.
export function bellEntries(
  env: EnvWarning[],
  instance: { plainHttp: boolean; keyBackup: boolean; proxy: Warning[] },
): Entry[] {
  const offline = env.filter((w) => w.id === "env_offline");
  const collapse = offline.length > COLLAPSE_OFFLINE_ABOVE;
  const entries: Entry[] = [];
  if (collapse) {
    const newest =
      offline
        .map((w) => w.since ?? "")
        .sort()
        .at(-1) || undefined;
    entries.push({
      kind: "manyOffline",
      key: "many-offline",
      severity: "error",
      since: newest,
      count: offline.length,
    });
  }
  for (const w of env) {
    if (collapse && w.id === "env_offline") continue;
    entries.push({ kind: "env", key: entryKey(w), severity: w.severity, since: w.since, w });
  }
  if (instance.plainHttp)
    entries.push({ kind: "plainHttp", key: "plain_http", severity: "warning" });
  for (const w of instance.proxy) {
    entries.push({ kind: "proxy", key: w.id, severity: "warning", w });
  }
  if (instance.keyBackup)
    entries.push({ kind: "keyBackup", key: "key_backup", severity: "warning" });

  const byNewest = (a: Entry, b: Entry) => Date.parse(b.since ?? "") - Date.parse(a.since ?? "");
  const group = (severity: Entry["severity"]) => {
    const all = entries.filter((e) => e.severity === severity);
    const dated = all.filter((e) => e.since && !Number.isNaN(Date.parse(e.since)));
    return [...dated.sort(byNewest), ...all.filter((e) => !dated.includes(e))];
  };
  return [...group("error"), ...group("warning")];
}

const linkClass =
  "text-accent-ink underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-sm";

const touchLink = "max-md:inline-flex max-md:min-h-11 max-md:items-center";

// The ways an environment item can act, so the item stays a plain description of one.
interface ItemActionsProps {
  // Closes the popover, which a link inside it does when it is followed.
  close: () => void;
  // Opens the install dialog of an environment in its "Show install command" form.
  finishSetup: (envId: string, from: HTMLElement) => void;
  canFinishSetup: (envId: string) => boolean;
}

// The time left on a certificate, as a person says it: "in 6 days", "in 3 hours".
function untilText(iso: string, nowMs: number): string {
  const left = Date.parse(iso) - nowMs;
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: "always" });
  const hour = 3_600_000;
  if (left >= 24 * hour) return rtf.format(Math.floor(left / (24 * hour)), "day");
  if (left >= hour) return rtf.format(Math.floor(left / hour), "hour");
  return rtf.format(Math.max(1, Math.floor(left / 60_000)), "minute");
}

// One bell item: a title that truncates with its whole text on hover, then its
// description. Everything an agent supplied is a React string, never markup (D-39).
function BellItem({ id, title, children }: { id: string; title: string; children: ReactNode }) {
  return (
    <Item variant="outline" size="sm" role="listitem" data-warning={id}>
      <ItemContent className="min-w-0">
        <ItemTitle className="block max-w-full truncate" title={title}>
          {title}
        </ItemTitle>
        {children}
      </ItemContent>
    </Item>
  );
}

function EnvEntry({ w, close, finishSetup, canFinishSetup }: { w: EnvWarning } & ItemActionsProps) {
  const t = useT();
  const now = useNow();
  const envId = w.envId ?? "";
  const name = w.envName ?? "";
  const node = w.node ?? "";

  switch (w.id) {
    case "env_offline": {
      const time = formatClock(w.since);
      const lead = time ? t("bell.env.offline.lead", { time }) : t("bell.env.offline.leadNoTime");
      return (
        <BellItem id={w.id} title={t("bell.env.offline.title", { name })}>
          {w.reason ? (
            <ReasonText
              variant="compact"
              code={w.reason}
              lead={lead}
              params={{ environment: name }}
              className="wrap-anywhere [&>p:first-child]:text-muted-foreground"
            />
          ) : (
            <ItemDescription className="line-clamp-none wrap-anywhere">{lead}</ItemDescription>
          )}
          <Link
            to="/env/$envId"
            params={{ envId }}
            className={`${linkClass} text-sm ${touchLink}`}
            onClick={close}
          >
            {t("bell.env.offline.view")}
          </Link>
        </BellItem>
      );
    }
    case "cert_not_renewing": {
      const expired = Date.parse(w.expiresAt ?? "") <= now;
      const when = expired
        ? formatRelative(w.expiresAt ?? "", new Date(now), t)
        : untilText(w.expiresAt ?? "", now);
      return (
        <BellItem id={w.id} title={t("bell.env.cert.title")}>
          <ItemDescription className="line-clamp-none wrap-anywhere">
            {t(expired ? "bell.env.cert.bodyExpired" : "bell.env.cert.body", {
              node,
              env: name,
              when,
            })}
          </ItemDescription>
          <Link
            to="/settings/environments/$envId"
            params={{ envId }}
            className={`${linkClass} text-sm ${touchLink}`}
            onClick={close}
          >
            {t("bell.env.cert.view")}
          </Link>
        </BellItem>
      );
    }
    case "manual_update_needed":
      return (
        <BellItem id={w.id} title={t("bell.env.manualUpdate.title", { count: w.count ?? 0 })}>
          <ItemDescription className="line-clamp-none wrap-anywhere">
            {t("bell.env.manualUpdate.body")}
          </ItemDescription>
          {envId ? (
            <Link
              to="/settings/environments/$envId"
              params={{ envId }}
              className={`${linkClass} text-sm ${touchLink}`}
              onClick={close}
            >
              {t("bell.env.manualUpdate.view")}
            </Link>
          ) : (
            <Link
              to="/settings/environments"
              className={`${linkClass} text-sm ${touchLink}`}
              onClick={close}
            >
              {t("bell.env.manualUpdate.view")}
            </Link>
          )}
        </BellItem>
      );
    case "clock_skew":
      return (
        <BellItem id={w.id} title={t("bell.env.clock.title", { node })}>
          <ItemDescription className="line-clamp-none wrap-anywhere">
            {t("bell.env.clock.body", {
              node,
              env: name,
              offset: `${Math.abs(Math.trunc(w.offsetSeconds ?? 0))} s`,
            })}
          </ItemDescription>
          <Link
            to="/settings/environments/$envId"
            params={{ envId }}
            className={`${linkClass} text-sm ${touchLink}`}
            onClick={close}
          >
            {t("bell.env.clock.view")}
          </Link>
        </BellItem>
      );
    case "pending_long":
      return (
        <BellItem id={w.id} title={t("bell.env.pending.title", { name })}>
          <ItemDescription className="line-clamp-none wrap-anywhere">
            {t("bell.env.pending.body", { count: w.days ?? 0 })}
          </ItemDescription>
          {canFinishSetup(envId) ? (
            <button
              type="button"
              className={`${linkClass} w-fit text-sm ${touchLink}`}
              onClick={(event) => finishSetup(envId, event.currentTarget)}
            >
              {t("bell.env.pending.finish")}
            </button>
          ) : (
            <Link
              to="/env/$envId"
              params={{ envId }}
              className={`${linkClass} text-sm ${touchLink}`}
              onClick={close}
            >
              {t("bell.env.pending.view")}
            </Link>
          )}
        </BellItem>
      );
    case "cluster_changed":
      return (
        <BellItem id={w.id} title={t("bell.env.clusterChanged.title", { name })}>
          <ItemDescription className="line-clamp-none wrap-anywhere">
            {t("bell.env.clusterChanged.body")}
          </ItemDescription>
          <Link
            to="/env/$envId"
            params={{ envId }}
            className={`${linkClass} text-sm ${touchLink}`}
            onClick={close}
          >
            {t("bell.env.clusterChanged.view")}
          </Link>
        </BellItem>
      );
    case "fold_offered":
      return (
        <BellItem id={w.id} title={t("bell.env.fold.title", { swarm: name })}>
          <ItemDescription className="line-clamp-none wrap-anywhere">
            {t("bell.env.fold.body")}
          </ItemDescription>
          <Link
            to="/settings/environments/$envId"
            params={{ envId }}
            hash="fold"
            className={`${linkClass} text-sm ${touchLink}`}
            onClick={close}
          >
            {t("bell.env.fold.review")}
          </Link>
        </BellItem>
      );
    default:
      // A problem this build has no wording for says nothing rather than something wrong.
      return null;
  }
}

// HealthBell is the header bell: a count of what needs attention and a
// "Warnings" popover that lists it (D-59). It merges the instance warnings (an
// admin's), the reverse proxy problems and the environment problems of every
// environment the caller may read (D-39). The data-not-persisted warning has its own
// banner, so it is not repeated here.
export function HealthBell() {
  const t = useT();
  const queryClient = useQueryClient();
  const { data: me } = useMe();
  const { items, loading: instanceLoading } = useWarnings();
  const { items: envItems, loading: envLoading } = useEnvWarnings();
  const [acknowledging, setAcknowledging] = useState(false);
  const [open, setOpen] = useState(false);
  const [install, setInstall] = useState<{ open: boolean; envId?: string }>({ open: false });
  const bell = useRef<HTMLButtonElement>(null);
  const returnFocus = useRef<HTMLElement | null>(null);

  const plainHttp = isPlainHttpRemote();
  const keyBackup = items.some((w) => w.id === "key_backup");
  const proxyItems = items.filter((w) => proxyCopy(w.id) !== undefined);
  const entries = bellEntries(envItems, { plainHttp, keyBackup, proxy: proxyItems });
  const loading = instanceLoading || envLoading;
  const count = loading ? 0 : entries.length;

  async function acknowledge() {
    setAcknowledging(true);
    try {
      const { response } = await api.POST("/api/warnings/key-backup/ack");
      if (!response.ok) {
        toastError(t("common.mutationFailed"));
        return;
      }
      await queryClient.invalidateQueries({ queryKey: WARNINGS_KEY });
    } catch {
      toastError(t("common.mutationFailed"));
    } finally {
      setAcknowledging(false);
    }
  }

  const actions: ItemActionsProps = {
    close: () => setOpen(false),
    finishSetup: (envId, from) => {
      // The dialog hands focus back to what opened it; the popover is gone by
      // then, so that is the bell.
      returnFocus.current = bell.current ?? from;
      setOpen(false);
      setInstall({ open: true, envId });
    },
    canFinishSetup: (envId) => hasPermission(me, "environments.token", envId),
  };

  return (
    <>
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger asChild>
          <Button
            ref={bell}
            variant="ghost"
            size="icon"
            className="relative"
            aria-label={
              count > 0 ? t("shell.warnings.bellCount", { count }) : t("shell.warnings.bell")
            }
          >
            <Bell aria-hidden="true" />
            {count > 0 ? (
              <Badge
                aria-hidden="true"
                className="absolute -top-0.5 -right-0.5 h-4 min-w-4 justify-center rounded-full bg-status-warning px-1 text-[10px] text-neutral-950"
              >
                {count > BADGE_CAP ? `${BADGE_CAP}+` : count}
              </Badge>
            ) : null}
          </Button>
        </PopoverTrigger>
        <PopoverContent
          align="end"
          className="w-80"
          onOpenAutoFocus={(event) => {
            // The popover takes focus itself, so Tab reaches the first item. Left to
            // itself it focuses the first button in the list, which is wherever a
            // "Finish setup" happens to be.
            event.preventDefault();
            if (event.currentTarget instanceof HTMLElement) event.currentTarget.focus();
          }}
        >
          <PopoverTitle>{t("shell.warnings.title")}</PopoverTitle>
          {loading ? (
            <Item variant="outline" size="sm" aria-hidden="true" data-loading="true">
              <ItemContent className="min-w-0">
                <Skeleton className="h-4 w-3/4" />
                <Skeleton className="h-4 w-full" />
              </ItemContent>
            </Item>
          ) : entries.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("shell.warnings.empty")}</p>
          ) : (
            // The list scrolls inside 320px (UI-SPEC). The viewport's own wrapper is a
            // table, which would widen to its longest line; as a block it wraps.
            <ScrollArea className="[&_[data-slot=scroll-area-viewport]]:max-h-80 [&_[data-slot=scroll-area-viewport]>div]:block!">
              <ItemGroup className="gap-2">
                {entries.map((entry) => {
                  switch (entry.kind) {
                    case "env":
                      return <EnvEntry key={entry.key} w={entry.w} {...actions} />;
                    case "manyOffline":
                      return (
                        <BellItem
                          key={entry.key}
                          id="many_offline"
                          title={t("bell.env.manyOffline.title", { count: entry.count })}
                        >
                          <ItemDescription className="line-clamp-none wrap-anywhere">
                            {t("bell.env.manyOffline.body")}
                          </ItemDescription>
                          <Link
                            to="/environments"
                            className={`${linkClass} text-sm ${touchLink}`}
                            onClick={actions.close}
                          >
                            {t("bell.env.manyOffline.view")}
                          </Link>
                        </BellItem>
                      );
                    case "plainHttp":
                      return (
                        <Item
                          key={entry.key}
                          variant="outline"
                          size="sm"
                          role="listitem"
                          data-warning="plain_http"
                        >
                          <ItemContent className="min-w-0">
                            <ItemTitle>{t("shell.warnings.plainHttp.title")}</ItemTitle>
                            <ItemDescription>
                              <a
                                href={__BAYPLAN_SOURCE_URL__}
                                target="_blank"
                                rel="noopener noreferrer"
                                className={linkClass}
                              >
                                {t("shell.warnings.plainHttp.fix")}
                              </a>
                            </ItemDescription>
                          </ItemContent>
                        </Item>
                      );
                    case "proxy": {
                      const key = proxyCopy(entry.w.id);
                      if (!key) return null;
                      return (
                        <Item
                          key={entry.key}
                          variant="outline"
                          size="sm"
                          role="listitem"
                          data-warning={entry.w.id}
                        >
                          <ItemContent className="min-w-0">
                            <ItemTitle>{t(`bell.proxy.${key}.title`)}</ItemTitle>
                            <ItemDescription className="line-clamp-none wrap-anywhere">
                              {t(`bell.proxy.${key}.body`, {
                                ip: entry.w.ip ?? "",
                                host: entry.w.host ?? "",
                                scheme: entry.w.scheme ?? "",
                                url: entry.w.url ?? "",
                              })}
                            </ItemDescription>
                            <a
                              href={`${__BAYPLAN_SOURCE_URL__}${PROXY_DOCS_PATH}`}
                              target="_blank"
                              rel="noopener noreferrer"
                              className={`${linkClass} text-sm`}
                            >
                              {t("bell.proxy.docs")}
                            </a>
                          </ItemContent>
                        </Item>
                      );
                    }
                    case "keyBackup":
                      return (
                        <Item
                          key={entry.key}
                          variant="outline"
                          size="sm"
                          role="listitem"
                          data-warning="key_backup"
                        >
                          <ItemContent className="min-w-0">
                            <ItemTitle>{t("shell.warnings.keyBackup.title")}</ItemTitle>
                            <ItemDescription>{t("shell.warnings.keyBackup.body")}</ItemDescription>
                          </ItemContent>
                          <ItemActions className="w-full justify-end">
                            <Button
                              size="sm"
                              variant="outline"
                              className="max-md:h-11"
                              disabled={acknowledging}
                              onClick={acknowledge}
                            >
                              {t("shell.warnings.keyBackup.ack")}
                            </Button>
                          </ItemActions>
                        </Item>
                      );
                    default:
                      return null;
                  }
                })}
              </ItemGroup>
            </ScrollArea>
          )}
        </PopoverContent>
      </Popover>
      <InstallDialog
        open={install.open}
        mode="rotate"
        envId={install.envId}
        returnFocusRef={returnFocus}
        onOpenChange={(next) => setInstall((current) => ({ ...current, open: next }))}
      />
    </>
  );
}

// DataNotPersistedBanner is the full-width strip above the header while the
// data directory would be lost with the container (D-15). It cannot be
// dismissed: the only fix is to mount a volume. Only admins see it.
export function DataNotPersistedBanner() {
  const t = useT();
  const { items } = useWarnings();
  if (!items.some((w) => w.id === "data_not_persisted")) return null;
  return (
    <div
      role="status"
      className="flex items-start gap-2 border-b border-status-warning bg-status-warning/15 px-4 py-2 text-sm text-status-warning-text md:px-6"
    >
      <TriangleAlert aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
      <p>{t("shell.warnings.banner")}</p>
    </div>
  );
}
