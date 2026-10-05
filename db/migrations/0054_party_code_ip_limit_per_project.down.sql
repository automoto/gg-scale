DROP FUNCTION party_code_ip_limit(text, boolean, integer, bigint);

TRUNCATE party_code_ip_attempts;

CREATE FUNCTION party_code_ip_limit(source_ip text, failed boolean) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path=public,pg_temp AS $$
DECLARE blocked boolean;
BEGIN
 INSERT INTO party_code_ip_attempts(ip) VALUES(source_ip) ON CONFLICT DO NOTHING;
 SELECT COALESCE(blocked_until>now(),false) INTO blocked FROM party_code_ip_attempts WHERE ip=source_ip FOR UPDATE;
 IF blocked THEN RETURN true; END IF;
 IF failed THEN
  UPDATE party_code_ip_attempts SET
   failures=CASE WHEN window_start<=now()-interval '15 minutes' THEN 1 ELSE failures+1 END,
   window_start=CASE WHEN window_start<=now()-interval '15 minutes' THEN now() ELSE window_start END,
   blocked_until=CASE WHEN window_start>now()-interval '15 minutes' AND failures+1>=100 THEN now()+interval '15 minutes' ELSE NULL END
  WHERE ip=source_ip;
 END IF;
 RETURN false;
END $$;
REVOKE ALL ON FUNCTION party_code_ip_limit(text,boolean) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION party_code_ip_limit(text,boolean) TO ggscale_app;
