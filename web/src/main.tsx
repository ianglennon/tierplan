// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import "@fontsource-variable/geist";
import "@fontsource-variable/geist-mono";
import "./index.css";
import { AuthLayout } from "@/components/auth-layout";
import i18n from "@/i18n";

const root = document.getElementById("root");
if (!root) throw new Error("missing #root element");

createRoot(root).render(
  <StrictMode>
    <AuthLayout title={i18n.t("common.brand.wordmark")} />
  </StrictMode>,
);
