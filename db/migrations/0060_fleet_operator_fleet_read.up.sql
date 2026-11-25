-- Fleet operators can open the Fleets pages: those GET routes check fleet
-- read, and fleet manage does not include it. Mirrors the rbac.defaultPolicyCSV
-- row added in the same change; the casbin-parity guard test enforces the match.
INSERT INTO casbin_rule (ptype, v0, v1, v2, v3)
VALUES ('p', 'role:fleet_operator', '*', 'project:*:fleet', 'read')
ON CONFLICT DO NOTHING;
