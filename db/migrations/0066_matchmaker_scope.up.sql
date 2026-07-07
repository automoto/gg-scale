-- Matchmaker becomes its own key scope, decoupled from fleet. Keys that
-- could matchmake before (fleet scope) keep working; new keys get the
-- matchmaker scope at creation time.
UPDATE api_keys
SET scopes = array_append(scopes, 'matchmaker')
WHERE 'fleet' = ANY (scopes)
  AND NOT ('matchmaker' = ANY (scopes));
