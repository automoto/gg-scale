CREATE INDEX leaderboard_entries_best_score_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, end_user_id, score DESC);

CREATE INDEX game_server_allocations_tenant_backend_idx
    ON game_server_allocations (tenant_id, backend);
