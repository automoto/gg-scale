-- Stamp every allocation with the fleet it came from. ON DELETE RESTRICT
-- because soft delete is the supported retirement path; a hard delete would
-- be an operator mistake we want to refuse rather than silently null out.

ALTER TABLE game_server_allocations
    ADD COLUMN fleet_id BIGINT REFERENCES fleets(id) ON DELETE RESTRICT;

CREATE INDEX game_server_allocations_fleet_id_idx
    ON game_server_allocations (fleet_id);
