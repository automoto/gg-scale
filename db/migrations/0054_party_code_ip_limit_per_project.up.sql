-- The party-code IP budget is now per project: the caller passes a
-- "project:ip" key, so one project's failed guesses no longer block valid
-- joins in another project from the same IP (shared NAT, proxies). The
-- failure limit and cooldown come from server config instead of literals.
DROP FUNCTION party_code_ip_limit(text, boolean);

-- Rows keyed by the bare IP are never read again.
TRUNCATE party_code_ip_attempts;

CREATE FUNCTION party_code_ip_limit(source_key text, failed boolean, max_failures integer, cooldown_secs bigint) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=public,pg_temp AS $$
DECLARE
 blocked boolean;
 cooldown interval := make_interval(secs => cooldown_secs);
BEGIN
 INSERT INTO party_code_ip_attempts(ip) VALUES(source_key) ON CONFLICT DO NOTHING;
 SELECT COALESCE(blocked_until>now(),false) INTO blocked FROM party_code_ip_attempts WHERE ip=source_key FOR UPDATE;
 IF blocked THEN RETURN true; END IF;
 IF failed THEN
  UPDATE party_code_ip_attempts SET
   failures=CASE WHEN window_start<=now()-cooldown THEN 1 ELSE failures+1 END,
   window_start=CASE WHEN window_start<=now()-cooldown THEN now() ELSE window_start END,
   blocked_until=CASE WHEN window_start>now()-cooldown AND failures+1>=max_failures THEN now()+cooldown ELSE NULL END
  WHERE ip=source_key;
 END IF;
 RETURN false;
END $$;
REVOKE ALL ON FUNCTION party_code_ip_limit(text,boolean,integer,bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION party_code_ip_limit(text,boolean,integer,bigint) TO ggscale_app;
