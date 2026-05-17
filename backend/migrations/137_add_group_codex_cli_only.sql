-- Add codex_cli_only restriction flag to groups.
-- When enabled, the group only accepts requests from official Codex client family
-- (codex-tui, codex_vscode, Codex Desktop, etc.).
-- Applies to OpenAI platform groups, analogous to claude_code_only for Anthropic groups.

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS codex_cli_only boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN groups.codex_cli_only IS 'Whether to restrict this group to official Codex clients only (OpenAI platform).';
