// @vitest-environment jsdom

import type { ReactNode } from "react";
import { describe, it, expect, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import type { ProviderUsageResponse } from "@multica/core/types";
import { I18nProvider } from "@multica/core/i18n/react";
import enCommon from "../../locales/en/common.json";
import enRuntimes from "../../locales/en/runtimes.json";

const TEST_RESOURCES = { en: { common: enCommon, runtimes: enRuntimes } };

const queryResult = vi.hoisted(() => ({
  current: {
    data: { providers: [] } as ProviderUsageResponse,
    isLoading: false,
  },
}));

vi.mock("@tanstack/react-query", async () => {
  const actual =
    await vi.importActual<typeof import("@tanstack/react-query")>(
      "@tanstack/react-query",
    );
  return {
    ...actual,
    useQuery: () => queryResult.current,
  };
});

vi.mock("@multica/core/runtimes/queries", () => ({
  runtimeProviderUsageOptions: () => ({ kind: "provider-usage" }),
}));

import { ProviderUsageBlock } from "./provider-usage-block";

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <I18nProvider locale="en" resources={TEST_RESOURCES}>
      {children}
    </I18nProvider>
  );
}

function formatWhen(value: string): string {
  return new Intl.DateTimeFormat("en", {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

describe("ProviderUsageBlock", () => {
  it("shows percent, reset, plan, and collected time", () => {
    const collectedAt = "2026-09-21T07:00:00.000Z";
    const resetsAt = "2026-09-22T07:59:00.000Z";
    queryResult.current = {
      isLoading: false,
      data: {
        providers: [
          {
            provider: "claude",
            plan_name: "Max",
            collected_at: collectedAt,
            windows: [
              { id: "session", percent_used: 38.2, resets_at: resetsAt },
              { id: "weekly_all", percent_used: 4, resets_at: resetsAt },
            ],
          },
        ],
      },
    };

    render(<ProviderUsageBlock wsId="ws-1" runtimeId="rt-1" />, {
      wrapper: Wrapper,
    });

    expect(screen.getByText("Claude Code")).toBeInTheDocument();
    expect(screen.getByText("Plan: Max")).toBeInTheDocument();
    expect(screen.getByText("Session")).toBeInTheDocument();
    expect(screen.getByText("Weekly")).toBeInTheDocument();
    expect(screen.getByText("38% used")).toBeInTheDocument();
    expect(screen.getByText("4% used")).toBeInTheDocument();
    expect(
      screen.getByText(`Collected ${formatWhen(collectedAt)}`),
    ).toBeInTheDocument();
    expect(screen.getAllByText(`Resets ${formatWhen(resetsAt)}`)).toHaveLength(
      2,
    );
  });

  it("shows the empty-session reason when a provider has no windows", () => {
    queryResult.current = {
      isLoading: false,
      data: {
        providers: [
          {
            provider: "codex",
            reason_code: "api_key_only",
            windows: [],
          },
        ],
      },
    };

    render(<ProviderUsageBlock wsId="ws-1" runtimeId="rt-1" />, {
      wrapper: Wrapper,
    });

    expect(screen.getByText("Codex")).toBeInTheDocument();
    expect(
      screen.getByText(
        "This Codex login is an API key, so plan limits are not available.",
      ),
    ).toBeInTheDocument();
  });

  it("keeps an unknown provider label and falls back for an unknown reason", () => {
    queryResult.current = {
      isLoading: false,
      data: {
        providers: [{ provider: "mystery", reason_code: "brand_new" }],
      },
    };

    render(<ProviderUsageBlock wsId="ws-1" runtimeId="rt-1" />, {
      wrapper: Wrapper,
    });

    expect(screen.getByText("mystery")).toBeInTheDocument();
    expect(
      screen.getByText("No local session on this machine."),
    ).toBeInTheDocument();
  });

  it("tolerates a snapshot that omits windows", () => {
    queryResult.current = {
      isLoading: false,
      data: {
        providers: [{ provider: "cursor", plan_name: "pro" }],
      },
    };

    render(<ProviderUsageBlock wsId="ws-1" runtimeId="rt-1" />, {
      wrapper: Wrapper,
    });

    expect(screen.getByText("Cursor")).toBeInTheDocument();
    expect(screen.getByText("Plan: pro")).toBeInTheDocument();
    expect(
      screen.getByText("No local session on this machine."),
    ).toBeInTheDocument();
  });

  it("shows the waiting copy when the machine has not reported yet", () => {
    queryResult.current = { isLoading: false, data: { providers: [] } };

    render(<ProviderUsageBlock wsId="ws-1" runtimeId="rt-1" />, {
      wrapper: Wrapper,
    });

    expect(
      screen.getByText("No plan usage reported from this machine yet."),
    ).toBeInTheDocument();
  });
});
