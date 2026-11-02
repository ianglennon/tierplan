// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { TriangleAlert } from "lucide-react";
import { useMemo } from "react";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { unshownPaths } from "@/features/service-editor/form-paths";
import type { SpecValue } from "@/features/spec-editor/spec-format";
import { useT } from "@/i18n/use-t";

// How many paths the bar names itself; the rest are in the popover.
const NAMED = 3;

interface CautionBarProps {
  spec: SpecValue;
  // Switches to the Spec tab.
  onOpenSpec?: () => void;
}

// CautionBar tells a person that the spec holds settings no control of the Form shows (D-02):
// saving from the form keeps them as they are, and the Spec tab is where they are seen. It is
// there exactly when the spec has such a setting, computed from the spec as it is now, and it
// names the first three paths, with the rest in a popover.
export function CautionBar({ spec, onOpenSpec }: CautionBarProps) {
  const t = useT();
  const paths = useMemo(() => unshownPaths(spec), [spec]);
  if (paths.length === 0) return null;
  const named = paths.slice(0, NAMED);
  const more = paths.length - named.length;
  return (
    <Alert variant="warning" role="note">
      <TriangleAlert aria-hidden="true" />
      <AlertDescription className="flex flex-col gap-1 text-status-warning-text">
        <p>{t("serviceForm.caution.body")}</p>
        <p className="flex flex-wrap items-baseline gap-x-1 font-mono text-xs break-all">
          <span>{t("serviceForm.caution.notShown", { paths: named.join(", ") })}</span>
          {more > 0 ? (
            <Popover>
              <PopoverTrigger asChild>
                <Button
                  type="button"
                  variant="link"
                  size="sm"
                  className="h-auto p-0 font-sans text-xs max-md:min-h-11"
                >
                  {t("serviceForm.caution.more", { count: more })}
                </Button>
              </PopoverTrigger>
              <PopoverContent align="start" className="max-h-80 w-96 max-w-[90vw] overflow-y-auto">
                <ul
                  aria-label={t("serviceForm.caution.list")}
                  className="flex flex-col gap-1 font-mono text-xs break-all"
                >
                  {paths.map((p) => (
                    <li key={p}>{p}</li>
                  ))}
                </ul>
              </PopoverContent>
            </Popover>
          ) : null}
        </p>
        <div>
          <Button
            type="button"
            variant="link"
            size="sm"
            className="h-auto p-0 max-md:min-h-11"
            onClick={onOpenSpec}
          >
            {t("serviceForm.caution.open")}
          </Button>
        </div>
      </AlertDescription>
    </Alert>
  );
}
