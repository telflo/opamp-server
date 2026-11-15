# Upgrading

**Take a backup of the database before every `migrate up`.** `serve` refuses
a schema newer than its build, so rolling back a release means restoring
that backup: you can't roll a migration back in production.

Each release, and the schema it brings a database to:

| Release | Schema |
|---|---|
| 0.2.4 | 13 |

0.2.4 is the first public release. From the next one on, a section here
for each release says what its upgrade needs from you.

## Unreleased: the database's new names

The database, schema and roles have new names (`telflo_opamp_server`,
`opamp_server`, `telflo_opamp_server_owner`,
`telflo_opamp_server_runtime`, `telflo_opamp_server`), and the
migrations make the new names from the start. A 0.2.4 database keeps
the old ones, and this release's `serve` refuses it. Set up a new
database with this release's bootstrap and `migrate up`
([database.md](database.md)), then register your organizations and
publish your configurations again; collectors enroll again with new
tokens.
