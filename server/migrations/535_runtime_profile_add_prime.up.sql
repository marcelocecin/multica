-- Add Prime Agent (`prime`) to the built-in runtime profile protocol whitelist.
-- Kept in lockstep with agent.SupportedTypes and agent.New(). NOT VALID
-- preserves the historical-row tolerance used by the prior family additions.
-- Renumbered from the fork's 265 slot, which upstream already used for
-- issue views, so this runs after the current whitelist (migration 441).
ALTER TABLE runtime_profile DROP CONSTRAINT IF EXISTS runtime_profile_protocol_family_check;

ALTER TABLE runtime_profile ADD CONSTRAINT runtime_profile_protocol_family_check
    CHECK (protocol_family IN (
        'claude',
        'codebuddy',
        'codex',
        'copilot',
        'opencode',
        'codearts',
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
