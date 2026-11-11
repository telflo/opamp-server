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

## Unreleased: the new name

The image and the chart moved to `ghcr.io/telflo/telflo-opamp-server` and
`oci://ghcr.io/telflo/charts/telflo-opamp-server`: point your references at
them. Moving a Helm release from the old chart to the new one changes its
resources' default names, the bundled Postgres's volume claim included, so
set `fullnameOverride` to the old name (`<release>-opamp-server`) to keep
them.
