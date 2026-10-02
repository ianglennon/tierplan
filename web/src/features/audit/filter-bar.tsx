// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { CalendarDays, ChevronDown, Download, Users } from "lucide-react";
import { useId, useState } from "react";
import type { DateRange } from "react-day-picker";

import { auditActorsQuery } from "@/api/audit";
import { Button } from "@/components/ui/button";
import { Calendar } from "@/components/ui/calendar";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useT } from "@/i18n/use-t";
import {
  type AuditSearch,
  actionLabel,
  DEFAULT_RANGE,
  exportUrl,
  formatDay,
  KNOWN_ACTIONS,
  parseDay,
  RANGES,
  type Range,
} from "./filters";

interface FilterBarProps {
  search: AuditSearch;
  // Merges changes into the URL's search params. A key set to undefined leaves
  // the URL.
  onChange: (patch: Partial<AuditSearch>) => void;
  onClear: () => void;
}

// hasFilters is true when the view is not the default one, which is when
// "Clear filters" has something to clear.
export function hasFilters(search: AuditSearch): boolean {
  return (
    Boolean(search.user) ||
    (search.action?.length ?? 0) > 0 ||
    (search.range ?? DEFAULT_RANGE) !== DEFAULT_RANGE
  );
}

// UserFilter is a combobox over the actors the log has seen, and it takes free
// text too: a name that is not in the list (for example the attempted name of
// a failed sign-in) can still be searched for. It is a popover with a text
// field and a list of buttons, so it needs no command-palette package.
function UserFilter({
  value,
  onChange,
}: {
  value: string | undefined;
  onChange: (user: string | undefined) => void;
}) {
  const t = useT();
  const id = useId();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState("");
  const actors = useQuery(auditActorsQuery);
  const typed = draft.trim();
  const matches = (actors.data ?? []).filter((name) =>
    name.toLowerCase().includes(typed.toLowerCase()),
  );
  const isNew = typed !== "" && !(actors.data ?? []).includes(typed);

  function choose(user: string | undefined) {
    onChange(user);
    setOpen(false);
  }

  return (
    <div className="flex flex-col gap-1">
      <Label id={`${id}-label`} htmlFor={`${id}-trigger`} className="text-xs text-muted-foreground">
        {t("audit.filters.user")}
      </Label>
      <Popover
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          if (next) setDraft(value ?? "");
        }}
      >
        <PopoverTrigger asChild>
          <Button
            id={`${id}-trigger`}
            // The name is the label and the current value: "User Any user".
            // The trigger's own id is Radix's, so the value has its own.
            aria-labelledby={`${id}-label ${id}-value`}
            variant="outline"
            className="w-44 justify-between font-normal max-md:h-11"
          >
            <span id={`${id}-value`} className="truncate">
              {value ?? t("audit.filters.userPlaceholder")}
            </span>
            <Users aria-hidden="true" className="text-muted-foreground" />
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-64">
          <Input
            aria-label={t("audit.filters.userSearch")}
            placeholder={t("audit.filters.userSearch")}
            value={draft}
            maxLength={128}
            autoComplete="off"
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && typed !== "") {
                e.preventDefault();
                choose(typed);
              }
            }}
          />
          <ul
            aria-label={t("audit.filters.userList")}
            className="flex max-h-56 flex-col overflow-y-auto"
          >
            <li>
              <Button
                variant="ghost"
                size="sm"
                className="w-full justify-start font-normal"
                onClick={() => choose(undefined)}
              >
                {t("audit.filters.userAny")}
              </Button>
            </li>
            {isNew ? (
              <li>
                <Button
                  variant="ghost"
                  size="sm"
                  className="w-full justify-start font-normal"
                  onClick={() => choose(typed)}
                >
                  <span className="truncate">{t("audit.filters.userUse", { name: typed })}</span>
                </Button>
              </li>
            ) : null}
            {matches.map((name) => (
              <li key={name}>
                <Button
                  variant="ghost"
                  size="sm"
                  aria-pressed={name === value}
                  className="w-full justify-start font-normal aria-pressed:bg-muted"
                  onClick={() => choose(name)}
                >
                  <span className="truncate">{name}</span>
                </Button>
              </li>
            ))}
            {matches.length === 0 && !isNew ? (
              <li className="px-2 py-1.5 text-sm text-muted-foreground">
                {t("audit.filters.noUsers")}
              </li>
            ) : null}
          </ul>
        </PopoverContent>
      </Popover>
    </div>
  );
}

// ActionFilter is the multi-select of action types: a popover with a checkbox
// per known action, each with its human label.
function ActionFilter({
  value,
  onChange,
}: {
  value: string[];
  onChange: (action: string[] | undefined) => void;
}) {
  const t = useT();
  const id = useId();
  // A key in the URL that this build has no label for stays visible and can be
  // unticked, so a linked view with a newer action is not stuck.
  const keys = [...KNOWN_ACTIONS, ...value.filter((a) => !KNOWN_ACTIONS.includes(a))];

  function toggle(key: string, on: boolean) {
    const next = on ? [...value, key] : value.filter((a) => a !== key);
    onChange(next.length > 0 ? next : undefined);
  }

  return (
    <div className="flex flex-col gap-1">
      <Label id={`${id}-label`} htmlFor={`${id}-trigger`} className="text-xs text-muted-foreground">
        {t("audit.filters.action")}
      </Label>
      <Popover>
        <PopoverTrigger asChild>
          <Button
            id={`${id}-trigger`}
            // The name is the label and the current value: "User Any user".
            // The trigger's own id is Radix's, so the value has its own.
            aria-labelledby={`${id}-label ${id}-value`}
            variant="outline"
            className="w-44 justify-between font-normal max-md:h-11"
          >
            <span id={`${id}-value`} className="truncate">
              {value.length > 0
                ? t("audit.filters.actionCount", { count: value.length })
                : t("audit.filters.actionAny")}
            </span>
            <ChevronDown aria-hidden="true" className="text-muted-foreground" />
          </Button>
        </PopoverTrigger>
        <PopoverContent align="start" className="w-72">
          <fieldset className="flex max-h-72 flex-col gap-1 overflow-y-auto">
            <legend className="sr-only">{t("audit.filters.actionList")}</legend>
            {keys.map((key) => {
              const boxId = `${id}-${key}`;
              return (
                <div
                  key={key}
                  className="flex items-center gap-2 rounded-md px-1.5 py-1 max-md:py-2"
                >
                  <Checkbox
                    id={boxId}
                    checked={value.includes(key)}
                    onCheckedChange={(on) => toggle(key, on === true)}
                  />
                  <Label htmlFor={boxId} className="flex-1 cursor-pointer font-normal">
                    {actionLabel(key, t)}
                  </Label>
                </div>
              );
            })}
          </fieldset>
        </PopoverContent>
      </Popover>
    </div>
  );
}

// RangeFilter is the preset select and, for "Custom range…", a range calendar
// popover. The days are calendar days in the browser's time zone, both
// included (UI-SPEC); the URL holds them as YYYY-MM-DD.
function RangeFilter({
  search,
  onChange,
}: {
  search: AuditSearch;
  onChange: (patch: Partial<AuditSearch>) => void;
}) {
  const t = useT();
  const id = useId();
  const range = search.range ?? DEFAULT_RANGE;
  const [calendarOpen, setCalendarOpen] = useState(false);
  const from = parseDay(search.from);
  const to = parseDay(search.to);
  const selected: DateRange | undefined = from || to ? { from, to } : undefined;
  const day = new Intl.DateTimeFormat(undefined, { dateStyle: "medium" });
  const summary =
    from || to
      ? t("audit.filters.customSummary", {
          from: from ? day.format(from) : "…",
          to: to ? day.format(to) : "…",
        })
      : t("audit.filters.customOpenEnded");

  function pickRange(next: Range) {
    if (next === "custom") {
      onChange({ range: next });
      setCalendarOpen(true);
      return;
    }
    // A preset has no days of its own, so they leave the URL.
    onChange({ range: next, from: undefined, to: undefined });
  }

  return (
    <div className="flex flex-col gap-1">
      <Label id={`${id}-label`} htmlFor={`${id}-range`} className="text-xs text-muted-foreground">
        {t("audit.filters.range")}
      </Label>
      <div className="flex flex-wrap items-center gap-2">
        <Select value={range} onValueChange={(v) => pickRange(v as Range)}>
          <SelectTrigger
            id={`${id}-range`}
            aria-labelledby={`${id}-label ${id}-range`}
            className="w-44 max-md:h-11"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent position="popper">
            {RANGES.map((r) => (
              <SelectItem key={r} value={r}>
                {t(`audit.ranges.${r}`)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {range === "custom" ? (
          <Popover open={calendarOpen} onOpenChange={setCalendarOpen}>
            <PopoverTrigger asChild>
              <Button
                variant="outline"
                aria-label={t("audit.filters.customLabel")}
                className="font-normal max-md:h-11"
              >
                <CalendarDays aria-hidden="true" className="text-muted-foreground" />
                <span>{summary}</span>
              </Button>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-auto p-0">
              <p className="px-3 pt-3 text-xs text-muted-foreground">
                {t("audit.filters.customDialog")}
              </p>
              <Calendar
                mode="range"
                numberOfMonths={1}
                defaultMonth={from ?? to}
                selected={selected}
                onSelect={(r) =>
                  onChange({
                    range: "custom",
                    from: r?.from ? formatDay(r.from) : undefined,
                    to: r?.to ? formatDay(r.to) : undefined,
                  })
                }
              />
            </PopoverContent>
          </Popover>
        ) : null}
      </div>
    </div>
  );
}

// ExportMenu offers the two downloads. Each item is a plain link to the
// export endpoint with the same filters as the list, so it saves exactly the
// entries on screen; it is a GET, so the session cookie goes with it and no
// CSRF token is needed. The address is rebuilt as the item is activated, so a
// preset such as "Last hour" counts back from that moment.
function ExportMenu({ search }: { search: AuditSearch }) {
  const t = useT();
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="outline" className="max-md:h-11">
          <Download aria-hidden="true" />
          {t("audit.export.menu")}
          <ChevronDown aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-64">
        <DropdownMenuLabel className="text-xs font-normal text-muted-foreground">
          {t("audit.export.help")}
        </DropdownMenuLabel>
        {(["csv", "json"] as const).map((format) => (
          <DropdownMenuItem key={format} asChild className="max-md:min-h-11">
            <a
              href={exportUrl(format, search, new Date())}
              download
              onClick={(e) => {
                e.currentTarget.href = exportUrl(format, search, new Date());
              }}
            >
              {t(`audit.export.${format}`)}
            </a>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// FilterBar is the audit log's controls (D-43): user, action, time range,
// Clear filters and Export. Every change is written to the URL by the caller.
export function FilterBar({ search, onChange, onClear }: FilterBarProps) {
  const t = useT();
  return (
    <search
      aria-label={t("audit.filters.label")}
      className="flex flex-wrap items-end gap-x-4 gap-y-3"
    >
      <UserFilter value={search.user} onChange={(user) => onChange({ user })} />
      <ActionFilter value={search.action ?? []} onChange={(action) => onChange({ action })} />
      <RangeFilter search={search} onChange={onChange} />
      <Button
        variant="ghost"
        disabled={!hasFilters(search)}
        onClick={onClear}
        className="max-md:h-11"
      >
        {t("audit.filters.clear")}
      </Button>
      <div className="ml-auto">
        <ExportMenu search={search} />
      </div>
    </search>
  );
}
