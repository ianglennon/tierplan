// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Download } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Spinner } from "@/components/ui/spinner";
import { fromLocalInput, type ShowRange, toLocalInput } from "@/features/logs/log-prefs";
import { logRoute } from "@/features/logs/log-source";
import { type StreamTarget, sourceOf } from "@/features/logs/log-stream";
import { useT } from "@/i18n/use-t";
import { apiUrl } from "@/lib/base-path";
import { toastError } from "@/lib/toast";

// How long the button shows its spinner after the download was handed to the browser.
export const DOWNLOAD_SPINNER_MS = 1000;

const PRESET_MS = { "5m": 5 * 60_000, "1h": 3_600_000, "24h": 86_400_000 } as const;

// What a download reads: a container on its node, given as containerId (and nodeId), or any
// source, a service among them (plan 06-31, D-35). A container caller keeps using containerId.
export interface DownloadTarget extends StreamTarget {
  envId: string;
}

// downloadUrl is the address of the server's download of a range (D-18). It is a plain
// GET authorised by the session cookie, so the browser can save it as it streams.
export function downloadUrl(
  target: DownloadTarget,
  range: { since?: string; until?: string },
  timestamps: boolean,
): string {
  const src = sourceOf(target);
  const q = new URLSearchParams();
  if (src.kind === "container" && src.nodeId) q.set("nodeId", src.nodeId);
  if (range.since) q.set("since", range.since);
  if (range.until) q.set("until", range.until);
  // A service's or a stack's file always starts each line with its task and the UTC time.
  if (src.kind === "container" && timestamps) q.set("timestamps", "1");
  const query = q.toString();
  return apiUrl(
    `/api/env/${encodeURIComponent(target.envId)}/${logRoute(src)}/${encodeURIComponent(src.id)}/logs/download${query ? `?${query}` : ""}`,
  );
}

// downloadNoteKey is the note under the form: what the file holds and where it comes from.
function downloadNoteKey(kind: "container" | "service" | "stack"): string {
  if (kind === "stack") return "stackLogs.download.note";
  return kind === "service" ? "logs.download.serviceNote" : "logs.download.note";
}

// defaultRange is the From and To the form opens with: the range the viewer shows, in
// the browser's zone. "Last 500 lines" and "All" have no time to name, so both ends
// are empty and mean the whole log.
export function defaultRange(show: ShowRange, now: Date): { from: string; to: string } {
  switch (show.kind) {
    case "last":
      return {
        from: toLocalInput(new Date(now.getTime() - PRESET_MS[show.preset]).toISOString()),
        to: "",
      };
    case "custom":
      return { from: toLocalInput(show.from), to: show.to ? toLocalInput(show.to) : "" };
    default:
      return { from: "", to: "" };
  }
}

// startBrowserDownload hands an address to the browser as a download. The file is
// named and saved by the browser from the server's answer, so nothing is held here.
function startBrowserDownload(url: string): void {
  const a = document.createElement("a");
  a.href = url;
  a.download = "";
  a.rel = "noopener";
  a.style.display = "none";
  document.body.append(a);
  try {
    a.click();
  } finally {
    a.remove();
  }
}

export interface DownloadFormProps {
  target: DownloadTarget;
  // The viewer's range, for the two ends the form opens with.
  show: ShowRange;
  // The viewer's Timestamps setting, for the box the form opens with.
  timestamps: boolean;
  onCancel: () => void;
}

// DownloadForm is the range and the options of a download. It is mounted when its
// popover or dialog opens, so the ends it shows are the viewer's range at that moment.
function DownloadForm({ target, show, timestamps, onCancel }: DownloadFormProps) {
  const t = useT();
  const id = useId();
  const kind = sourceOf(target).kind;
  // A service's and a stack's file always carry the time, so the box is a container's alone.
  const isMerged = kind !== "container";
  const initial = useMemo(() => defaultRange(show, new Date()), [show]);
  const [from, setFrom] = useState(initial.from);
  const [to, setTo] = useState(initial.to);
  const [withStamps, setWithStamps] = useState(timestamps);
  const [pending, setPending] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  const fromIso = from ? fromLocalInput(from) : undefined;
  const toIso = to ? fromLocalInput(to) : undefined;
  const endBeforeStart =
    fromIso !== undefined && toIso !== undefined && Date.parse(toIso) <= Date.parse(fromIso);

  const submit = () => {
    if (pending || endBeforeStart) return;
    try {
      startBrowserDownload(downloadUrl(target, { since: fromIso, until: toIso }, withStamps));
    } catch {
      toastError(t("logs.download.error"));
      return;
    }
    setPending(true);
    timer.current = setTimeout(() => setPending(false), DOWNLOAD_SPINNER_MS);
  };

  return (
    <form
      className="flex flex-col gap-3"
      aria-label={t("logs.download.title")}
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${id}-from`}>{t("logs.download.from")}</Label>
        <Input
          id={`${id}-from`}
          type="datetime-local"
          value={from}
          onChange={(e) => setFrom(e.target.value)}
        />
      </div>
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${id}-to`}>{t("logs.download.to")}</Label>
        <Input
          id={`${id}-to`}
          type="datetime-local"
          value={to}
          aria-invalid={endBeforeStart}
          aria-describedby={endBeforeStart ? `${id}-err` : undefined}
          onChange={(e) => setTo(e.target.value)}
        />
        {endBeforeStart ? (
          <p id={`${id}-err`} role="alert" className="text-xs text-destructive">
            {t("logs.range.invalid")}
          </p>
        ) : null}
      </div>
      <p className="text-xs text-muted-foreground">{t("logs.download.empty")}</p>
      {isMerged ? null : (
        <div className="flex items-center gap-2">
          <Checkbox
            id={`${id}-ts`}
            checked={withStamps}
            onCheckedChange={(v) => setWithStamps(v === true)}
          />
          <Label htmlFor={`${id}-ts`} className="font-normal">
            {t("logs.download.timestamps")}
          </Label>
        </div>
      )}
      <p className="text-xs text-muted-foreground">{t(downloadNoteKey(kind))}</p>
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" size="sm" onClick={onCancel}>
          {t("logs.download.cancel")}
        </Button>
        <Button type="submit" size="sm" disabled={endBeforeStart} aria-busy={pending}>
          {pending ? <Spinner aria-label={t("logs.download.starting")} /> : null}
          {t("logs.download.submit")}
        </Button>
      </div>
    </form>
  );
}

// DownloadPopover is the Download… button of the toolbar and the popover it opens.
export function DownloadPopover(props: Omit<DownloadFormProps, "onCancel">) {
  const t = useT();
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" size="sm">
          <Download aria-hidden="true" />
          {t("logs.download.trigger")}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80" aria-label={t("logs.download.title")}>
        <p className="mb-3 text-sm font-medium">{t("logs.download.title")}</p>
        <DownloadForm {...props} onCancel={() => setOpen(false)} />
      </PopoverContent>
    </Popover>
  );
}

// DownloadDialog is the same form as a dialog, for the More menu of a narrow screen,
// whose item cannot hold a popover.
export function DownloadDialog({
  open,
  onOpenChange,
  ...props
}: Omit<DownloadFormProps, "onCancel"> & {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const t = useT();
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-160">
        <DialogHeader>
          <DialogTitle>{t("logs.download.title")}</DialogTitle>
          <DialogDescription className="sr-only">
            {t(downloadNoteKey(sourceOf(props.target).kind))}
          </DialogDescription>
        </DialogHeader>
        <DownloadForm {...props} onCancel={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}
