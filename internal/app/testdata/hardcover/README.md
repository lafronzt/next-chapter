# Hardcover response contracts

`connection.json` is a synthetic response, based on the field contract in
`prompts/39-hardcover-connection.md` and the related integration prompts.
It contains no real account data and is not a recording of a live API call.
The documentation home at https://docs.hardcover.app/ was reachable during
implementation; the old `/api/` index returned 404. No operator token was used.

The connection action checks these fields at runtime before saving a link.
Changes to these contracts must be checked against the current schema and
accompanied by fixture and schema-drift test updates.
