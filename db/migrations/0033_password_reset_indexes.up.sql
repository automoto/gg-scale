-- Account-scoped partial indexes so the "burn every outstanding link" update
-- that runs with each password change stays an index scan, not a table scan.
-- Retention is the password_reset_gc periodic job (expired rows deleted after
-- a day); these tables must not grow forever.
CREATE INDEX control_panel_password_resets_user_open_idx
    ON control_panel_password_resets (control_panel_user_id)
    WHERE used_at IS NULL;

CREATE INDEX player_account_password_resets_account_open_idx
    ON player_account_password_resets (player_account_id)
    WHERE used_at IS NULL;
