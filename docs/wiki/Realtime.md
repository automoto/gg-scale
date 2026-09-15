# Realtime

The existing `matchmaker_matched` event includes the full roster. Each roster
member can include `party_id` and has `queue_entry_id`. Delivery is best effort.
After reconnect, get `/v1/parties/current`, find your member `ticket_id`, and poll
`/v1/matchmaker/tickets/{ticket_id}`. Party presence uses the explicit party
heartbeat endpoint; a WebSocket connection alone does not extend its deadline.
