// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Eye, EyeOff } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

// Eight dots whatever the value, so its length is not given away.
const DOTS = "••••••••";

type MaskedValueProps = {
  // The variable's name, so a table of them has toggles that can be told apart.
  name?: string;
} & (
  | {
      // The caller may not reveal. There is no value to pass: the server withheld it
      // (redact.Env), and a browser that was never sent a secret cannot leak it.
      canReveal?: false;
      value?: undefined;
    }
  | {
      // The caller holds secrets.reveal on the environment.
      canReveal: true;
      value: string;
    }
);

// MaskedValue is a secret value as the inspect views show it (D-15a, UI-SPEC
// "Component Inventory"): •••••••• with a tooltip saying why. For a holder of
// secrets.reveal it gains a "Show value" toggle (aria-pressed) that swaps the
// dots for the value and back. Masking is the server's job; this only draws what
// the server sent, so a person who may not reveal is never given a value to show.
export function MaskedValue(props: MaskedValueProps) {
  const t = useT();
  const [shown, setShown] = useState(false);
  const reveal = props.canReveal === true;

  return (
    <span className="inline-flex min-w-0 items-center gap-2">
      {reveal && shown ? (
        <span className="min-w-0 font-mono text-sm break-all">{props.value}</span>
      ) : (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
              tabIndex={0}
              className="rounded-sm font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              {DOTS}
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-sm">
            {reveal ? t("owned.mask.hiddenReveal") : t("owned.mask.hidden")}
          </TooltipContent>
        </Tooltip>
      )}
      {reveal ? (
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="shrink-0 max-md:size-11"
          aria-pressed={shown}
          aria-label={
            props.name ? t("owned.mask.showNamed", { name: props.name }) : t("owned.mask.show")
          }
          onClick={() => setShown((v) => !v)}
        >
          {shown ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
        </Button>
      ) : null}
    </span>
  );
}
