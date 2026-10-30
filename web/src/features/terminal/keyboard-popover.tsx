// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Keyboard } from "lucide-react";
import { useId } from "react";

import { Button } from "@/components/ui/button";
import { Kbd } from "@/components/ui/kbd";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Switch } from "@/components/ui/switch";
import { useT } from "@/i18n/use-t";
import { isMacPlatform } from "@/lib/env-nav";

interface KeyboardPopoverProps {
  screenReader: boolean;
  onScreenReaderChange: (on: boolean) => void;
}

// KeyboardPopover lists the few keys the terminal keeps for itself, and the way out of
// it: the terminal takes every other key, Tab and Esc included, so without a documented
// chord it would be a keyboard trap (UI-SPEC "Keyboard trap escape"). It also holds the
// switch for screen reader support.
export function KeyboardPopover({ screenReader, onScreenReaderChange }: KeyboardPopoverProps) {
  const t = useT();
  const id = useId();
  const mac = isMacPlatform();
  const rows: { key: string; label: string; keys: string }[] = [
    { key: "copy", label: t("terminal.keyboard.copy"), keys: mac ? "⌘C" : "Ctrl+Shift+C" },
    { key: "paste", label: t("terminal.keyboard.paste"), keys: mac ? "⌘V" : "Ctrl+Shift+V" },
    { key: "search", label: t("terminal.keyboard.search"), keys: "Ctrl+Shift+F" },
    { key: "leave", label: t("terminal.keyboard.leave"), keys: "Ctrl+Shift+Home" },
  ];
  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" size="sm" className="max-md:h-11">
          <Keyboard aria-hidden="true" />
          {t("terminal.keyboard.trigger")}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-80 max-w-[calc(100vw-2rem)]">
        <dl className="flex flex-col gap-2 text-sm">
          {rows.map((r) => (
            <div key={r.key} className="flex items-center justify-between gap-3">
              <dt>{r.label}</dt>
              <dd>
                <Kbd>{r.keys}</Kbd>
              </dd>
            </div>
          ))}
        </dl>
        <p className="mt-3 text-xs text-muted-foreground">{t("terminal.keyboard.copyOnSelect")}</p>
        <div className="mt-3 flex items-center justify-between gap-3 border-t pt-3">
          <Label htmlFor={`${id}-sr`} className="text-sm">
            {t("terminal.keyboard.screenReader")}
          </Label>
          <Switch
            id={`${id}-sr`}
            checked={screenReader}
            onCheckedChange={onScreenReaderChange}
            className="max-md:scale-125"
          />
        </div>
      </PopoverContent>
    </Popover>
  );
}
