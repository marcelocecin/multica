-- Table is 589. Concurrent indexes are 590 and 591. Upstream main owns
-- 583 (runtime_profile_add_muse), 584 (quick_action_optional_target),
-- 585 (issue_workflow_owner), 586 (cloud_agent_connection, #9402) and
-- 587 (cloud_agent_connection_provider_index). Sibling PR #6641 takes 588.
-- Derived plan-limit snapshots reported by a local daemon. Tokens, cookies,
-- and auth.json contents are not columns on purpose: the daemon uploads
-- only provider, window, percent, reset, plan, and collected_at.
CREATE TABLE runtime_provider_usage_snapshot (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    runtime_id UUID NOT NULL,
    provider TEXT NOT NULL,
    window_id TEXT NOT NULL DEFAULT '',
    percent_used DOUBLE PRECISION,
    resets_at TIMESTAMPTZ,
    plan_name TEXT,
    collected_at TIMESTAMPTZ NOT NULL,
    reason_code TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
