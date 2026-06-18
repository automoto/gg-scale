CREATE INDEX leaderboard_entries_best_score_asc_idx
    ON leaderboard_entries (tenant_id, leaderboard_id, end_user_id, score ASC);
