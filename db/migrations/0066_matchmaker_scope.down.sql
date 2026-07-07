UPDATE api_keys
SET scopes = array_remove(scopes, 'matchmaker')
WHERE 'matchmaker' = ANY (scopes);
