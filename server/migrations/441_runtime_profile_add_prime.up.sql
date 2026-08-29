-- Add Prime Agent (`prime`) to the built-in runtime profile protocol whitelist.
-- Kept in lockstep with agent.SupportedTypes and agent.New().  NOT VALID
-- preserves the historical-row tolerance used by the prior family additions.
--
-- Rebased onto migration 403 (add zeroclaw), the newest family migration on
-- main.  This statement replaces the whole constraint rather than amending it,
-- so any family missing from the list below is revoked: `zeroclaw`
-- (migration 403), `dim` (migration 370) and `mcode` (migration 342) are
-- therefore listed explicitly, not inherited.
--
-- The number matters as much as the list.  The migration runner applies
-- versions out of order (see internal/migrations.AllVersions), so a prefix
-- below 403 would run *after* zeroclaw on any database that already applied
-- it, and this rewritten CHECK would silently revoke the zeroclaw family.
-- 441 is above every version currently on main: 440 was free when this was
-- last renumbered, and v0.4.36 has since taken it for
-- 440_github_pr_head_sha_index (#7695).
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'openclaw',
        'hermes',
        'pi',
        'cursor',
        'kimi',
        'reasonix',
        'dsh',
        'kiro',
        'antigravity',
        'qoder',
        'qoderclicn',
        'traecli',
        'deveco',
        'grok',
        'qwen',
        'qwenpaw',
        'mcode',
        'dim',
        'zeroclaw',
        'prime'
    )) NOT VALID;
