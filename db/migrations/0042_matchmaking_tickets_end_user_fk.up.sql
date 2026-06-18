ALTER TABLE matchmaking_tickets
    ADD CONSTRAINT matchmaking_tickets_end_user_id_fkey
    FOREIGN KEY (end_user_id)
    REFERENCES end_users(id)
    ON DELETE CASCADE
    NOT VALID;
