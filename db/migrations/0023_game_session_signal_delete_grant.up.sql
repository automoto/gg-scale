-- The periodic GC sweep prunes expired game_session_signal rows so a
-- long-lived session can't accumulate them until it ends. The 0022 grant was
-- SELECT, INSERT only (signals are append-only on the request path); the sweep
-- needs DELETE.
GRANT DELETE ON game_session_signal TO ggscale_app;
