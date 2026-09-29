-- An account without a password has only provider sign-in. Restoring NOT NULL
-- would need an invented hash, so refuse instead of locking those owners out.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM player_accounts WHERE password_hash IS NULL)
       OR EXISTS (SELECT 1 FROM control_panel_users WHERE password_hash IS NULL) THEN
        RAISE EXCEPTION 'cannot revert sso_connections: accounts without a password exist; set a password on them or delete them first';
    END IF;
END $$;

DROP TABLE control_panel_user_connections;
DROP TABLE player_account_connections;

ALTER TABLE control_panel_users ALTER COLUMN password_hash SET NOT NULL;
ALTER TABLE player_accounts ALTER COLUMN password_hash SET NOT NULL;
