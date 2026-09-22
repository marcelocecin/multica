-- Derived plan-limit snapshots for one runtime. No foreign keys: runtime
-- and workspace cleanup delete these rows in the same statement as the parent.

-- name: DeleteRuntimeProviderUsage :exec
DELETE FROM runtime_provider_usage_snapshot
WHERE runtime_id = $1 AND workspace_id = $2 AND provider = $3;

-- name: InsertRuntimeProviderUsage :exec
INSERT INTO runtime_provider_usage_snapshot (
    workspace_id,
    runtime_id,
    provider,
    window_id,
    percent_used,
    resets_at,
    plan_name,
    collected_at,
    reason_code
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
);

-- name: ListRuntimeProviderUsage :many
SELECT provider, window_id, percent_used, resets_at, plan_name, collected_at, reason_code
FROM runtime_provider_usage_snapshot
WHERE workspace_id = $1 AND runtime_id = $2
ORDER BY provider, window_id;
