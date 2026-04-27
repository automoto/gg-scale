-- Phase 0 initial migration: extensions only. Tables land in Phase 1.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;
