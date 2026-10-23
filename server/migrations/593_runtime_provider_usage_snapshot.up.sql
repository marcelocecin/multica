-- Table is 593. Concurrent indexes are 594 and 595. Upstream main owns
-- 583 (runtime_profile_add_muse), 584 (quick_action_optional_target),
-- 585 (issue_workflow_owner), 586 (cloud_agent_connection, #9402),
-- 587 (cloud_agent_connection_provider_index), 588
-- (588_daemon_token_owner, #9423), 589 (589_member_delegation_day),
-- 590 (590_member_delegation_day_unique_index) and 591
-- (591_member_delegation_day_primary_key, #9435). Sibling PR #6641 takes 592
-- (592_runtime_profile_add_prime). These files were 590, 591 and 592 before
-- that collision.
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
