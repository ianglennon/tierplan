// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { seedUpdateConfig } from "@/features/service-editor/update-policy";
import type { SpecValue } from "@/features/spec-editor/spec-format";

// defaultServiceSpec is the spec the Create service page opens with (D-04, UI-SPEC "Create
// defaults"): replicated, one replica, restart on any exit after 5 seconds, update one task at a
// time stopping each first with no delay, no limits, no networks, secrets, configs or ports. The
// name and the image are empty and required. It is a new object each time, so one page's edits
// never reach the next.
export function defaultServiceSpec(): SpecValue {
  return seedUpdateConfig({
    Name: "",
    TaskTemplate: {
      ContainerSpec: { Image: "" },
      RestartPolicy: { Condition: "any", Delay: 5_000_000_000 },
    },
    Mode: { Replicated: { Replicas: 1 } },
  });
}
