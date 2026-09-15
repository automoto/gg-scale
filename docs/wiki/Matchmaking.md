# Matchmaking

A queue entry is indivisible. Solo entries contain one ticket; party entries
contain the ready party roster. Workers claim and group whole entries. Matches
can combine several parties and solo fill. Roster members carry `party_id` and
`queue_entry_id`. A rematch preserves the party ID and excludes solo fill from
party membership. See the [cutover procedure](../parties.md#operator-cutover).
