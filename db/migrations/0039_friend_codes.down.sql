DROP INDEX project_players_friend_code_uniq;

ALTER TABLE project_players
DROP COLUMN friend_code;
