ALTER TABLE audit_log
    DROP CONSTRAINT IF EXISTS audit_log_actor_user_id_fkey;

ALTER TABLE audit_log
    ADD CONSTRAINT audit_log_actor_user_id_fkey
    FOREIGN KEY (actor_user_id)
    REFERENCES end_users(id)
    ON DELETE SET NULL
    NOT VALID;

ALTER TABLE audit_log
    VALIDATE CONSTRAINT audit_log_actor_user_id_fkey;
