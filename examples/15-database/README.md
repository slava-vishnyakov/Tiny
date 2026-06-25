# 15 Database

This example demonstrates the `db` module — a driver-agnostic layer over SQL
databases, modelled on the `bun:sqlite` API. It ships with three pure-Go
drivers: **sqlite**, **postgres**, and **mysql**.

### Features shown:
- `db.open(driver, dsn)` for SQLite, PostgreSQL, and MySQL.
- Positional (`?`, array) and named (`:key`, object) parameters. For PostgreSQL
  these are automatically rebound to `$1, $2, ...`.
- `run` / `query` / `get` and the `{ changes, lastInsertRowid }` result.
- Explicit, pool-safe transactions via `begin` / `commit` / `rollback`.
- Reusable prepared statements via `prepare`.
- Catchable errors with `try` / `catch`.

### Running

The main example uses an in-memory SQLite database and needs no setup:

```bash
cd examples/15-database
../../tiny
```

The `postgres.tiny` and `mysql.tiny` scripts target external servers — edit the
DSN at the top of each file to point at your database, then:

```bash
../../tiny postgres.tiny
../../tiny mysql.tiny
```
