// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useId, useState } from "react";

import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  type CardProps,
  FormCard,
  TOUCH_HEIGHT,
  useReport,
  useTouched,
} from "@/features/service-editor/cards/form-card";
import {
  bytesToMemory,
  coresToNano,
  type MemoryUnit,
  memoryToBytes,
  nanoToCores,
} from "@/features/service-editor/field-values";
import { getPath } from "@/features/service-editor/spec-patch";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

const CPU_LIMIT = "TaskTemplate.Resources.Limits.NanoCPUs";
const MEMORY_LIMIT = "TaskTemplate.Resources.Limits.MemoryBytes";
const CPU_RESERVATION = "TaskTemplate.Resources.Reservations.NanoCPUs";
const MEMORY_RESERVATION = "TaskTemplate.Resources.Reservations.MemoryBytes";

interface MemoryState {
  text: string;
  unit: MemoryUnit;
}

// ResourcesCard: CPU in cores (stored as NanoCPUs) and memory in MiB or GiB (stored as bytes),
// each as a limit and a reservation. Empty means no limit. Each field writes only its own path,
// so a limit the form does not show (Pids) and the other fields are never touched.
export function ResourcesCard({ spec, set, unset, report }: CardProps) {
  const t = useT();
  const ids = useId();
  const [seen, touch] = useTouched();
  const [cpu, setCpu] = useState({
    limit: nanoToCores(getPath(spec, CPU_LIMIT)),
    reservation: nanoToCores(getPath(spec, CPU_RESERVATION)),
  });
  const [memory, setMemory] = useState<{ limit: MemoryState; reservation: MemoryState }>({
    limit: bytesToMemory(getPath(spec, MEMORY_LIMIT)),
    reservation: bytesToMemory(getPath(spec, MEMORY_RESERVATION)),
  });

  const cpuBad = (v: string) => v.trim() !== "" && coresToNano(v) === null;
  const memoryBad = (m: MemoryState) =>
    m.text.trim() !== "" && memoryToBytes(m.text, m.unit) === null;
  const invalid =
    (cpuBad(cpu.limit) && seen("cpuLimit")) ||
    (cpuBad(cpu.reservation) && seen("cpuReservation")) ||
    (memoryBad(memory.limit) && seen("memoryLimit")) ||
    (memoryBad(memory.reservation) && seen("memoryReservation"));
  useReport("resources", report, invalid);

  // Writing a size: a number is set, and an empty field (or 0, which Docker reads as no limit)
  // takes the setting out of the spec. A field with no setting that is left empty writes nothing.
  function write(path: string, value: number | null) {
    if (value !== null && value > 0) set(path, value);
    else unset(path);
  }

  function onCpu(which: "limit" | "reservation", text: string) {
    setCpu((prev) => ({ ...prev, [which]: text }));
    touch(which === "limit" ? "cpuLimit" : "cpuReservation");
    const path = which === "limit" ? CPU_LIMIT : CPU_RESERVATION;
    if (text.trim() === "") write(path, null);
    else {
      const nano = coresToNano(text);
      if (nano !== null) write(path, nano);
    }
  }

  function onMemory(which: "limit" | "reservation", next: MemoryState) {
    setMemory((prev) => ({ ...prev, [which]: next }));
    touch(which === "limit" ? "memoryLimit" : "memoryReservation");
    const path = which === "limit" ? MEMORY_LIMIT : MEMORY_RESERVATION;
    if (next.text.trim() === "") write(path, null);
    else {
      const bytes = memoryToBytes(next.text, next.unit);
      if (bytes !== null) write(path, bytes);
    }
  }

  function cpuField(which: "limit" | "reservation") {
    const id = `${ids}-cpu-${which}`;
    const label = t(
      which === "limit" ? "serviceForm.resources.cpuLimit" : "serviceForm.resources.cpuReservation",
    );
    const bad = cpuBad(cpu[which]) && seen(which === "limit" ? "cpuLimit" : "cpuReservation");
    return (
      <Field data-invalid={bad ? true : undefined}>
        <FieldLabel htmlFor={id}>{label}</FieldLabel>
        <Input
          id={id}
          value={cpu[which]}
          inputMode="decimal"
          onChange={(e) => onCpu(which, e.target.value)}
          autoComplete="off"
          aria-invalid={bad ? true : undefined}
          aria-describedby={`${id}-helper${bad ? ` ${id}-error` : ""}`}
          className={cn("font-mono", TOUCH_HEIGHT)}
        />
        <FieldDescription id={`${id}-helper`}>
          {t(
            which === "limit"
              ? "serviceForm.resources.limitHelper"
              : "serviceForm.resources.reservationHelper",
          )}
        </FieldDescription>
        {bad ? <FieldError id={`${id}-error`}>{t("serviceForm.errors.cores")}</FieldError> : null}
      </Field>
    );
  }

  function memoryField(which: "limit" | "reservation") {
    const id = `${ids}-memory-${which}`;
    const label = t(
      which === "limit"
        ? "serviceForm.resources.memoryLimit"
        : "serviceForm.resources.memoryReservation",
    );
    const unitLabel = t(
      which === "limit"
        ? "serviceForm.resources.memoryLimitUnit"
        : "serviceForm.resources.memoryReservationUnit",
    );
    const m = memory[which];
    const bad = memoryBad(m) && seen(which === "limit" ? "memoryLimit" : "memoryReservation");
    return (
      <Field data-invalid={bad ? true : undefined}>
        <FieldLabel htmlFor={id}>{label}</FieldLabel>
        <div className="flex gap-2">
          <Input
            id={id}
            value={m.text}
            inputMode="numeric"
            onChange={(e) => onMemory(which, { ...m, text: e.target.value })}
            autoComplete="off"
            aria-invalid={bad ? true : undefined}
            aria-describedby={`${id}-helper${bad ? ` ${id}-error` : ""}`}
            className={cn("min-w-0 flex-1 font-mono", TOUCH_HEIGHT)}
          />
          <Select
            value={m.unit}
            onValueChange={(unit) => onMemory(which, { ...m, unit: unit as MemoryUnit })}
          >
            <SelectTrigger aria-label={unitLabel} className={cn("w-24", TOUCH_HEIGHT)}>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="MiB">{t("serviceForm.resources.mib")}</SelectItem>
              <SelectItem value="GiB">{t("serviceForm.resources.gib")}</SelectItem>
            </SelectContent>
          </Select>
        </div>
        <FieldDescription id={`${id}-helper`}>
          {t(
            which === "limit"
              ? "serviceForm.resources.limitHelper"
              : "serviceForm.resources.reservationHelper",
          )}
        </FieldDescription>
        {bad ? <FieldError id={`${id}-error`}>{t("serviceForm.errors.whole")}</FieldError> : null}
      </Field>
    );
  }

  return (
    <FormCard title={t("serviceForm.resources.title")}>
      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        {cpuField("limit")}
        {cpuField("reservation")}
        {memoryField("limit")}
        {memoryField("reservation")}
      </div>
    </FormCard>
  );
}
