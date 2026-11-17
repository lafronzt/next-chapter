# Hardcover response contracts

`connection.json` is a synthetic response, based on the field contract in
`prompts/39-hardcover-connection.md` and the related integration prompts.
It contains no real account data and is not a recording of a live API call.
The documentation home at https://docs.hardcover.app/ was reachable during
implementation; the old `/api/` index returned 404. No operator token was used.

The connection action checks these fields at runtime before saving a link.
Changes to these contracts must be checked against the current schema and
accompanied by fixture and schema-drift test updates.


Note sharing uses the documented `insert_reading_journal(object: ...)` and
`update_reading_journal(id: ..., object: ...)` actions with `event: "note"`,
plain `entry`, explicit `privacy_setting_id` (3 private, 2 followers, 1 public),
and an empty `tags` list on insert. Responses have `errors` and
`reading_journal { id entry privacy_setting_id }`. The journal query is limited
to the current account, linked book and `note` events. This contract is from
https://docs.hardcover.app/api/guides/updatingreadingjournal/ and
https://docs.hardcover.app/api/graphql/schemas/readingjournals/ (checked
November 17, 2026). `hardcover_notes_test.go` supplies synthetic responses,
including refused and ambiguous writes; no real account was contacted.
