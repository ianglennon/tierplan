// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  CaseSensitive,
  ChevronDown,
  ChevronUp,
  Ellipsis,
  ExternalLink,
  Regex,
  Search,
  SlidersHorizontal,
} from "lucide-react";
import { type KeyboardEvent, type ReactNode, useId, useRef, useState } from "react";

import { LiveChip } from "@/components/live-chip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group";
import { Label } from "@/components/ui/label";
import { Popover, PopoverAnchor, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Toggle } from "@/components/ui/toggle";
import { fromLocalInput, type ShowRange, toLocalInput } from "@/features/logs/log-prefs";
import { useIsMobile } from "@/hooks/use-mobile";
import { useT } from "@/i18n/use-t";

export interface LogToolbarProps {
  show: ShowRange;
  onShowChange: (show: ShowRange) => void;

  query: string;
  onQueryChange: (query: string) => void;
  regex: boolean;
  onRegexChange: (on: boolean) => void;
  caseSensitive: boolean;
  onCaseSensitiveChange: (on: boolean) => void;
  onlyMatching: boolean;
  onOnlyMatchingChange: (on: boolean) => void;
  // The pattern is a regular expression that does not compile.
  invalidPattern: boolean;
  matchCount: number;
  // The match the counter is on, from 0.
  currentMatch: number;
  onNextMatch: () => void;
  onPreviousMatch: () => void;
  // The counter's words, spoken politely once typing has stopped; empty says nothing.
  announcement: string;

  wrap: boolean;
  onWrapChange: (on: boolean) => void;
  timestamps: boolean;
  onTimestampsChange: (on: boolean) => void;
  utc: boolean;
  onUtcChange: (on: boolean) => void;

  // Shown when the stream is connected or retrying; absent otherwise.
  live?: "live" | "reconnecting";
  // Buttons at the right of the second row (Download…, Open in new window), and the
  // items of the More menu that stand in for them under 768px. Without items there is
  // no More menu.
  actions?: ReactNode;
  moreItems?: ReactNode;
  // Opens the log in a window of its own. The button comes last in the second row, and
  // is an item of the More menu under 768px; without it there is none (the pop-out
  // does not offer a pop-out).
  onOpenPopout?: () => void;
}

// The toggles the arrow keys move between.
const ROVING = "[data-roving]:not([disabled])";

// LogToolbar is the log viewer's two rows of controls (UI-SPEC "Log viewer"): history
// and search in the first, the way the lines are drawn in the second. Under 768px the
// second row is the live chip, a View popover and a More menu. It holds no state of
// its own except what is open: the viewer owns every choice.
export function LogToolbar(p: LogToolbarProps) {
  const t = useT();
  const mobile = useIsMobile();
  const ref = useRef<HTMLDivElement>(null);

  // Arrow keys move between the toggles, as a toolbar does; Home and End go to the
  // ends. The search box is left alone, since the arrows edit its text.
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!["ArrowRight", "ArrowLeft", "Home", "End"].includes(e.key)) return;
    const target = e.target as HTMLElement;
    if (!target.matches("[data-roving]")) return;
    const items = Array.from(ref.current?.querySelectorAll<HTMLElement>(ROVING) ?? []);
    const at = items.indexOf(target);
    if (at < 0) return;
    let next = at;
    if (e.key === "ArrowRight") next = (at + 1) % items.length;
    else if (e.key === "ArrowLeft") next = (at - 1 + items.length) % items.length;
    else if (e.key === "Home") next = 0;
    else next = items.length - 1;
    e.preventDefault();
    items[next]?.focus();
  };

  const viewToggles = (
    <>
      <ViewToggle label={t("logs.wrap")} on={p.wrap} onChange={p.onWrapChange} />
      <ViewToggle label={t("logs.timestamps")} on={p.timestamps} onChange={p.onTimestampsChange} />
      <ViewToggle
        label={t("logs.utc")}
        on={p.utc && p.timestamps}
        disabled={!p.timestamps}
        onChange={p.onUtcChange}
      />
    </>
  );

  return (
    <div
      ref={ref}
      role="toolbar"
      aria-label={t("logs.toolbar")}
      aria-orientation="horizontal"
      onKeyDown={onKeyDown}
      className="flex flex-col gap-2"
    >
      <div className="flex flex-wrap items-end gap-2">
        <ShowSelect show={p.show} onChange={p.onShowChange} />
        <SearchGroup {...p} />
        <ViewToggle
          label={t("logs.search.only")}
          on={p.onlyMatching}
          onChange={p.onOnlyMatchingChange}
        />
      </div>
      <div className="flex min-h-8 flex-wrap items-center gap-2">
        {mobile ? (
          <>
            {p.live ? <LiveChip state={p.live} /> : null}
            <Popover>
              <PopoverTrigger asChild>
                <Button type="button" variant="outline" size="sm" className="max-md:h-11">
                  <SlidersHorizontal aria-hidden="true" />
                  {t("logs.view")}
                </Button>
              </PopoverTrigger>
              <PopoverContent align="start" className="w-56">
                {viewToggles}
              </PopoverContent>
            </Popover>
            {p.moreItems || p.onOpenPopout ? (
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button type="button" variant="outline" size="sm" className="max-md:h-11">
                    <Ellipsis aria-hidden="true" />
                    {t("logs.more")}
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end">
                  {p.moreItems}
                  {p.onOpenPopout ? (
                    <DropdownMenuItem onSelect={p.onOpenPopout}>
                      {t("logs.popout.open")}
                    </DropdownMenuItem>
                  ) : null}
                </DropdownMenuContent>
              </DropdownMenu>
            ) : null}
          </>
        ) : (
          <>
            {viewToggles}
            {p.live ? <LiveChip state={p.live} /> : null}
            <span className="flex-1" />
            {p.actions}
            {p.onOpenPopout ? (
              <Button type="button" variant="outline" size="sm" onClick={p.onOpenPopout}>
                <ExternalLink aria-hidden="true" />
                {t("logs.popout.open")}
              </Button>
            ) : null}
          </>
        )}
      </div>
      <span role="status" aria-live="polite" className="sr-only">
        {p.announcement}
      </span>
    </div>
  );
}

function ViewToggle({
  label,
  on,
  onChange,
  disabled,
}: {
  label: string;
  on: boolean;
  onChange: (on: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <Toggle
      data-roving=""
      variant="outline"
      size="sm"
      pressed={on}
      disabled={disabled}
      onPressedChange={onChange}
      className="max-md:h-11"
    >
      {label}
    </Toggle>
  );
}

// The Show select. "Custom…" opens a popover anchored to the select for the two ends of
// a range; cancelling it leaves the range as it was.
function ShowSelect({ show, onChange }: { show: ShowRange; onChange: (s: ShowRange) => void }) {
  const t = useT();
  const id = useId();
  const [customOpen, setCustomOpen] = useState(false);
  const value = show.kind === "last" ? show.preset : show.kind;

  const choose = (v: string) => {
    if (v === "custom") {
      setCustomOpen(true);
      return;
    }
    if (v === "lines" || v === "all") onChange({ kind: v });
    else if (v === "5m" || v === "1h" || v === "24h") onChange({ kind: "last", preset: v });
  };

  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id} className="text-xs text-muted-foreground">
        {t("logs.show.label")}
      </Label>
      <Popover open={customOpen} onOpenChange={setCustomOpen}>
        <PopoverAnchor asChild>
          <div>
            <Select value={value} onValueChange={choose}>
              <SelectTrigger id={id} className="w-44 max-md:h-11" aria-label={t("logs.show.label")}>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {(["lines", "5m", "1h", "24h", "all", "custom"] as const).map((k) => (
                  <SelectItem key={k} value={k}>
                    {t(`logs.show.${k}`)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </PopoverAnchor>
        <PopoverContent align="start" className="w-72">
          <CustomRange
            initial={show.kind === "custom" ? show : undefined}
            onCancel={() => setCustomOpen(false)}
            onApply={(r) => {
              setCustomOpen(false);
              onChange(r);
            }}
          />
        </PopoverContent>
      </Popover>
    </div>
  );
}

// The two ends of a custom range, as the browser's own date and time inputs read them,
// in the browser's time zone. The end is optional and must come after the start.
function CustomRange({
  initial,
  onCancel,
  onApply,
}: {
  initial?: Extract<ShowRange, { kind: "custom" }>;
  onCancel: () => void;
  onApply: (r: ShowRange) => void;
}) {
  const t = useT();
  const id = useId();
  const [from, setFrom] = useState(initial ? toLocalInput(initial.from) : "");
  const [to, setTo] = useState(initial?.to ? toLocalInput(initial.to) : "");
  const fromIso = fromLocalInput(from);
  const toIso = to ? fromLocalInput(to) : undefined;
  const endBeforeStart =
    fromIso !== undefined && toIso !== undefined && Date.parse(toIso) <= Date.parse(fromIso);
  const apply = () => {
    if (!fromIso || endBeforeStart) return;
    onApply({ kind: "custom", from: fromIso, ...(toIso ? { to: toIso } : {}) });
  };
  return (
    <form
      className="flex flex-col gap-2.5"
      aria-label={t("logs.range.title")}
      onSubmit={(e) => {
        e.preventDefault();
        apply();
      }}
    >
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${id}-from`}>{t("logs.range.from")}</Label>
        <Input
          id={`${id}-from`}
          type="datetime-local"
          value={from}
          onChange={(e) => setFrom(e.target.value)}
        />
      </div>
      <div className="flex flex-col gap-1">
        <Label htmlFor={`${id}-to`}>{t("logs.range.to")}</Label>
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
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" size="sm" onClick={onCancel}>
          {t("logs.range.cancel")}
        </Button>
        <Button type="submit" size="sm" disabled={!fromIso || endBeforeStart}>
          {t("logs.range.apply")}
        </Button>
      </div>
    </form>
  );
}

// The search box with Match case and Use regular expression inside it, the counter and
// the two buttons that move between matches. Enter goes to the next match and Shift+Enter
// to the one before.
function SearchGroup(p: LogToolbarProps) {
  const t = useT();
  const id = useId();
  const counter = counterText(p, t);
  // The toggles inside the box are real toggles with the accessible names the spec gives.
  const toggle = (label: string, on: boolean, onChange: (v: boolean) => void, icon: ReactNode) => (
    <Toggle
      data-roving=""
      size="sm"
      pressed={on}
      onPressedChange={onChange}
      aria-label={label}
      title={label}
      className="size-6 min-w-6 px-0"
    >
      {icon}
    </Toggle>
  );
  return (
    <div className="flex min-w-60 flex-1 flex-col gap-1">
      <Label htmlFor={id} className="text-xs text-muted-foreground">
        {t("logs.search.label")}
      </Label>
      <div className="flex items-center gap-2">
        <InputGroup className="max-w-md max-md:h-11">
          <InputGroupAddon>
            <Search aria-hidden="true" />
          </InputGroupAddon>
          <InputGroupInput
            id={id}
            type="search"
            value={p.query}
            placeholder={t("logs.search.placeholder")}
            aria-invalid={p.invalidPattern}
            aria-describedby={p.invalidPattern ? `${id}-err` : undefined}
            autoComplete="off"
            spellCheck={false}
            onChange={(e) => p.onQueryChange(e.target.value)}
            onKeyDown={(e) => {
              if (e.key !== "Enter") return;
              e.preventDefault();
              if (e.shiftKey) p.onPreviousMatch();
              else p.onNextMatch();
            }}
          />
          <InputGroupAddon align="inline-end">
            {toggle(
              t("logs.search.matchCase"),
              p.caseSensitive,
              p.onCaseSensitiveChange,
              <CaseSensitive aria-hidden="true" />,
            )}
            {toggle(t("logs.search.regex"), p.regex, p.onRegexChange, <Regex aria-hidden="true" />)}
          </InputGroupAddon>
        </InputGroup>
        {p.invalidPattern ? null : (
          <span className="min-w-20 text-sm text-muted-foreground tabular-nums">{counter}</span>
        )}
        <Button
          type="button"
          variant="outline"
          size="icon-sm"
          aria-label={t("logs.search.previous")}
          disabled={p.matchCount < 2}
          onClick={p.onPreviousMatch}
          className="max-md:size-11"
        >
          <ChevronUp aria-hidden="true" />
        </Button>
        <Button
          type="button"
          variant="outline"
          size="icon-sm"
          aria-label={t("logs.search.next")}
          disabled={p.matchCount < 2}
          onClick={p.onNextMatch}
          className="max-md:size-11"
        >
          <ChevronDown aria-hidden="true" />
        </Button>
      </div>
      {p.invalidPattern ? (
        <p id={`${id}-err`} role="alert" className="text-xs text-destructive">
          {t("logs.search.invalid")}
        </p>
      ) : null}
    </div>
  );
}

// "No matches", "1 of 1" or "{current} of {total}"; nothing before anything is typed.
export function counterText(
  p: Pick<LogToolbarProps, "query" | "matchCount" | "currentMatch" | "invalidPattern">,
  t: (key: string, options?: Record<string, unknown>) => string,
): string {
  if (p.query === "" || p.invalidPattern) return "";
  if (p.matchCount === 0) return t("logs.search.none");
  return t("logs.search.count", { current: p.currentMatch + 1, total: p.matchCount });
}
