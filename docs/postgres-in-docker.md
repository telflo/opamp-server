# PostgreSQL in Docker for opamp-server

This runs PostgreSQL 18 in a Docker container and sets it up for
opamp-server: the data in a Docker volume, the port open to this machine
only, and opamp-server's database and roles inside it. It ends with the two
database URLs opamp-server needs, saved as files.

It's step 2 of [Run it standalone](standalone.md).
Run it from that walkthrough's folder, `~/opamp-standalone`, which holds
`deploy/bootstrap.sql`.

It suits trying opamp-server out on a single machine. For a production
database, use a PostgreSQL your organization runs, with backups, monitoring
and TLS: [Using your own PostgreSQL instead](#using-your-own-postgresql-instead)
says what opamp-server needs from it.

**You need** Docker and `openssl`. You don't need `psql` on this machine:
the commands run in the container, using the container's own `psql`.

## 1. Choose three passwords

You need three passwords. The first password is for PostgreSQL's **superuser**,
which is used only to create opamp-server's database and its other two roles.
The other two passwords belong to the other two roles: an **owner**, which creates and
upgrades the tables, and a **read/write** role, which the running server
connects as. It can read and write the tables' rows but can't change the
tables themselves.

```sh
# Go to the walkthrough's folder, which only you can open.
cd ~/opamp-standalone

# One random password per role. Hex needs no escaping inside a URL.
for name in superuser-password owner-password runtime-password; do
  openssl rand -hex 24 > "$name"
done
```

**For your own:** any passwords will do. Percent-encode any character that
isn't a letter, a digit, `-`, `.`, `_` or `~` when you put it in a URL
(step 4).

## 2. Start PostgreSQL

```sh
# Give the superuser's password to Docker through the environment, which
# keeps it off the command line (and out of `ps`).
export POSTGRES_PASSWORD=$(cat superuser-password)

# Start PostgreSQL 18 in the background, restarted with Docker. Its data goes
# in the volume opamp-postgres-data, and its port is open to this machine only.
docker run -d --name opamp-postgres --restart unless-stopped \
  -e POSTGRES_PASSWORD \
  -v opamp-postgres-data:/var/lib/postgresql \
  -p 127.0.0.1:5432:5432 \
  postgres:18

# The password is in the container now: drop it from this shell.
unset POSTGRES_PASSWORD

# Wait until it accepts connections. The first start sets up the volume first.
until docker exec opamp-postgres pg_isready -h 127.0.0.1 -U postgres -q; do sleep 1; done
```

**For your own:**

- **If something already listens on port 5432:** publish another, such as
  `-p 127.0.0.1:5433:5432`, and use that port in step 4's URLs.
- **If opamp-server runs on another machine:** publish the port on an address
  that machine reaches, and limit who can reach it with a firewall.
  [configuration.md](configuration.md) explains `sslmode` in the URL for
  a database across a network.
- **If you have another version:** opamp-server needs PostgreSQL 16 or later, and is
  tested on 18. postgres:16 and postgres:17 keep their data in
  `/var/lib/postgresql/data`, so mount the volume there.

## 3. Create opamp-server's database and roles

[`deploy/bootstrap.sql`](../deploy/bootstrap.sql) creates the database
`telflo_opamp` and three roles. It runs as the superuser, once:

| Role | Logs in | What it's for |
|---|---|---|
| `telflo_opamp_owner` | yes | Owns the database and every table. `opamp-server migrate` runs as it. |
| `telflo_opamp_runtime` | no | Holds exactly the rights the running server needs, which the migrations grant it. |
| `telflo_opamp` | yes | `opamp-server serve` logs in as it. A member of `telflo_opamp_runtime`, so it has those rights and no others. |

```sh
# The script reads both passwords from its environment, never from arguments.
export OPAMP_OWNER_PASSWORD=$(cat owner-password) OPAMP_RUNTIME_PASSWORD=$(cat runtime-password)

# Run the script with the container's psql, as the superuser. `-e NAME`
# copies each password from this shell's environment into the container's.
docker exec -i -e OPAMP_OWNER_PASSWORD -e OPAMP_RUNTIME_PASSWORD opamp-postgres \
  psql -U postgres -d postgres -X -q -v ON_ERROR_STOP=1 -f - < deploy/bootstrap.sql

# Drop the passwords from this shell.
unset OPAMP_OWNER_PASSWORD OPAMP_RUNTIME_PASSWORD

# See what it made: the three roles, and the database.
docker exec opamp-postgres psql -U postgres -X -c '\du telflo_opamp*' -c '\l telflo_opamp'
```

The script is safe to run again: it creates only what's missing, and sets
both passwords again. That's also how you change a password: change its
file, then run the commands above again.

## 4. Save the two URLs

opamp-server reads its database from two settings, one per role:
`opamp-server migrate` uses `OPAMP_MIGRATION_DATABASE_URL`, as the owner,
and `opamp-server serve` uses `OPAMP_DATABASE_URL`, as the server's role.
Each refuses the other's role.

```sh
# The URL migrate uses: the database's owner.
echo "postgresql://telflo_opamp_owner:$(cat owner-password)@127.0.0.1:5432/telflo_opamp" > migration-database-url

# The URL serve uses: the server's own role, with at most 10 connections per process.
echo "postgresql://telflo_opamp:$(cat runtime-password)@127.0.0.1:5432/telflo_opamp?pool_max_conns=10" > database-url
```

Now go back to the walkthrough, [step 3](standalone.md#3-make-the-two-secrets).

## Day to day

```sh
# Stop PostgreSQL, and start it again. Docker also starts it when it starts.
docker stop opamp-postgres
docker start opamp-postgres

# Back up the database to a file. Take one before every `opamp-server migrate up`.
docker exec opamp-postgres pg_dump -U postgres -Fc telflo_opamp > "telflo_opamp-$(date +%Y%m%d-%H%M%S).dump"

# Restore a backup over the database, with both components stopped first.
docker exec -i opamp-postgres pg_restore -U postgres -d telflo_opamp --clean --if-exists --single-transaction < telflo_opamp-YYYYMMDD-HHMMSS.dump
```

A backup is also how you roll back a release: a migration can't be undone
in production ([upgrading.md](upgrading.md)).

**Remove it all**, the data included:

```sh
docker rm -f opamp-postgres && docker volume rm opamp-postgres-data
```

## Using your own PostgreSQL instead

Any PostgreSQL 16 or later works, on a server that also holds other
databases: opamp-server gets a database of its own. In place of the steps
above:

1. **Run the bootstrap once, as a superuser,** with `psql` 15 or later:

   ```sh
   export OPAMP_OWNER_PASSWORD=$(cat owner-password) OPAMP_RUNTIME_PASSWORD=$(cat runtime-password)
   psql "host=db.internal user=postgres dbname=postgres" -X -v ON_ERROR_STOP=1 -f deploy/bootstrap.sql
   unset OPAMP_OWNER_PASSWORD OPAMP_RUNTIME_PASSWORD
   ```

   On a managed PostgreSQL without a superuser (a cloud provider's admin
   role), don't run it: [database.md](database.md) says how to make the
   roles there.

2. **Save the two URLs** as in step 4, with your server's host and port.
   Across a network, add `sslmode=verify-full` to both
   ([configuration.md](configuration.md)).
