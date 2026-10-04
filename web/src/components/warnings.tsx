// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Bell, TriangleAlert } from "lucide-react";
import { useState } from "react";

import { api } from "@/api/client";
import type { components } from "@/api/schema";
import { WARNINGS_KEY } from "@/api/warnings";
import { isPlainHttpRemote } from "@/components/auth-layout";
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
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
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

// useWarnings reads the instance warnings the server knows about: the data
// directory is not persisted, and the generated encryption key has not been
// backed up. Only an admin may ask (settings.read), so for anyone else the
// query never runs and the list stays empty (D-59). The plain-HTTP warning is
// not here: only the browser knows how it reached the server.
export function useWarnings(): { items: Warning[]; allowed: boolean } {
  const allowed = usePermission("settings.read");
  const { data } = useQuery({
    queryKey: WARNINGS_KEY,
    enabled: allowed,
    refetchInterval: REFETCH_MS,
    queryFn: async () => {
      const { data: body } = await api.GET("/api/warnings");
      if (!body) throw new Error("GET /api/warnings failed");
      return body.items ?? [];
    },
  });
  return { items: allowed ? (data ?? []) : [], allowed };
}

const linkClass =
  "text-accent-ink underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-sm";

// HealthBell is the header bell: a count of what needs attention and a
// "Warnings" popover that lists it (D-59). The data-not-persisted warning has
// its own banner, so it is not repeated here.
export function HealthBell() {
  const t = useT();
  const queryClient = useQueryClient();
  const { items } = useWarnings();
  const [acknowledging, setAcknowledging] = useState(false);

  const plainHttp = isPlainHttpRemote();
  const keyBackup = items.some((w) => w.id === "key_backup");
  const proxyItems = items.filter((w) => proxyCopy(w.id) !== undefined);
  const count = (plainHttp ? 1 : 0) + (keyBackup ? 1 : 0) + proxyItems.length;

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

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button
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
              {count}
            </Badge>
          ) : null}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80">
        <PopoverTitle>{t("shell.warnings.title")}</PopoverTitle>
        {count === 0 ? (
          <p className="text-sm text-muted-foreground">{t("shell.warnings.empty")}</p>
        ) : (
          <ItemGroup className="gap-2">
            {plainHttp ? (
              <Item variant="outline" size="sm" role="listitem" data-warning="plain_http">
                <ItemContent>
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
            ) : null}
            {proxyItems.map((w) => {
              const key = proxyCopy(w.id);
              if (!key) return null;
              return (
                <Item key={w.id} variant="outline" size="sm" role="listitem" data-warning={w.id}>
                  <ItemContent>
                    <ItemTitle>{t(`bell.proxy.${key}.title`)}</ItemTitle>
                    <ItemDescription className="line-clamp-none wrap-anywhere">
                      {t(`bell.proxy.${key}.body`, {
                        ip: w.ip ?? "",
                        host: w.host ?? "",
                        scheme: w.scheme ?? "",
                        url: w.url ?? "",
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
            })}
            {keyBackup ? (
              <Item variant="outline" size="sm" role="listitem" data-warning="key_backup">
                <ItemContent>
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
            ) : null}
          </ItemGroup>
        )}
      </PopoverContent>
    </Popover>
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
