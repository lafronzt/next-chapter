# Hardcover request shapes

These are the mutation shapes this app sends, copied from Hardcover's official
TypeScript SDK (`@hardcover/sdk` 0.1.1, `dist/generated/graphql.js`). They are
not captured from a live account. If Hardcover changes them, the Hardcover
connection test (which introspects `user_books` and `user_book_reads`) and the
fake-server tests are where to look first.

`insert_user_book(object: UserBookCreateInput!)` and
`update_user_book(id: Int!, object: UserBookUpdateInput!)` both return
`{ id, error, user_book }`. A rejected change comes back with HTTP 200 and a
populated `error`, so the response body must always be checked.
