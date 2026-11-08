# Setting up the database

Three steps, in this order, each as a different role, so the running
server never holds more rights than it needs:

1. **bootstrap** (a PostgreSQL superuser, once): the roles and the
   database;
2. **`opamp-server migrate up`** (the database's owner): the schema and its
   tables, on every upgrade;
3. **`opamp-server serve`** (the server's own role): the management API
   and the agent endpoint ([configuration.md](configuration.md)).

`compose.yaml` runs all three, with development passwords. Use it as an
example, never as a deployment.

## 1. Bootstrap the database and roles

`deploy/bootstrap.sql` creates the roles and the database, which neither
the migrations nor the server should be able to create. Run it as a
superuser, connected to any existing database (`postgres` below). It isn't
in the image.

Pass the two passwords in the environment, **never with `-v`**: anyone on
the machine can see a command line with `ps`. Read them without echoing,
so they stay out of your shell's history too:

```sh
# Type the owner's password, then Enter, then the runtime role's, then Enter.
# Nothing is shown as you type.
read -rs OPAMP_OWNER_PASSWORD; export OPAMP_OWNER_PASSWORD
read -rs OPAMP_RUNTIME_PASSWORD; export OPAMP_RUNTIME_PASSWORD
psql "host=db.internal user=postgres dbname=postgres" -X \
  -v ON_ERROR_STOP=1 -f deploy/bootstrap.sql
# Drop both passwords from this shell.
unset OPAMP_OWNER_PASSWORD OPAMP_RUNTIME_PASSWORD
```

Through Docker, run this in place of the `psql` command, after the two
`read` lines. A bare `-e NAME` copies the value from your environment;
never write `-e NAME=value`, which is a command line again:

```sh
docker exec -i -e OPAMP_OWNER_PASSWORD -e OPAMP_RUNTIME_PASSWORD <container> \
  psql -U postgres -d postgres -X -v ON_ERROR_STOP=1 < deploy/bootstrap.sql
unset OPAMP_OWNER_PASSWORD OPAMP_RUNTIME_PASSWORD
```

The database is `telflo_opamp` unless you pass `-v database=<name>`. The
script stops before changing anything if either password is missing or
empty. It creates, each only if missing:

| Role | What it's for |
|---|---|
| `telflo_opamp_owner` | Logs in. Owns the database, the schema and every table. `migrate` runs as it. |
| `telflo_opamp_runtime` | Can't log in. Holds exactly the privileges the server needs, which the migrations grant it. |
| `telflo_opamp` | Logs in. `serve` runs as it. A member of `telflo_opamp_runtime`, and can't create or change tables. |

and the database, owned by `telflo_opamp_owner`, in UTF8. Only the two
login roles can connect to it. `telflo_opamp`'s `log_error_verbosity` is
`terse`, so PostgreSQL's own log doesn't quote the values in the server's
failed writes. `serve` refuses to start if the database isn't UTF8, if
`PUBLIC` can connect to it or make temporary tables in it, or if its
errors aren't terse.

You can run it again whenever you like. A rerun sets both passwords
again from the environment, so it's how you change one.

The passwords reach PostgreSQL inside `create role` and `alter role`
statements. A server with `log_statement` set to `ddl` or `all` writes
them to its log, and so does any server when such a statement fails.

### A managed Postgres without a superuser

A cloud provider's admin role isn't a superuser, so it can't run the
bootstrap. It stops part way, and the statement it stops at **writes the
runtime role's password to the server's log**. If you already tried, set a
new password for `telflo_opamp`.

Instead, as the admin role or with the provider's tools, follow the
bootstrap's statements as a checklist:

1. Create the three roles as the table above describes. Set each password
   the provider's way, never with a statement that might fail.
   `telflo_opamp` must not have `CREATEDB` or `CREATEROLE` (by default it
   doesn't); `alter role telflo_opamp nocreatedb nocreaterole` removes
   them.
2. Let the admin role act as the owner, `grant telflo_opamp_owner to
   <admin> with set true`, then create the database `owner
   telflo_opamp_owner`.
3. As the database's owner, run `revoke connect, temporary on database
   telflo_opamp from public`, then `grant connect on database telflo_opamp
   to telflo_opamp_owner, telflo_opamp`.
4. Make the server's errors terse. Only a superuser may set
   `log_error_verbosity`, so either:
   - set `log_error_verbosity = terse` server-wide in the provider's
     parameter group (or its equivalent); or
   - have the provider's superuser, where there is one, run `grant set on
     parameter log_error_verbosity to <admin>`, then as the admin run
     `alter role telflo_opamp set log_error_verbosity = terse`.

   Never grant `set` on it to the server's own role: it could set its
   errors back to verbose.

`serve` checks the result before it listens, and names the setting and the
statement that fixes it. It doesn't check `search_path`, so check that
yourself: connected to `telflo_opamp` as `telflo_opamp`, `select
current_setting('search_path')` must answer `telflo_opamp`.

## 2. Migrate

```sh
OPAMP_MIGRATION_DATABASE_URL=postgresql://telflo_opamp_owner:<password>@db.internal:5432/telflo_opamp \
  opamp-server migrate up      # or: status; or, in development: down --yes-drop-data
```

**Take a backup of the database before every `migrate up`.** `serve`
refuses a schema newer than its build, so rolling back a release means
restoring that backup. [upgrading.md](upgrading.md) has the notes for each
upgrade.

- **`up`** creates the schema if it's missing, then applies every pending
  migration, each in its own transaction. Run it before the first `serve`
  and before rolling out each new release. Two runs at once take turns.
- **`status`** shows each migration's state and the database's version.
- **`down --yes-drop-data`** rolls back the newest migration and drops its
  tables with every row in them. It's for development only.

`migrate` runs **only as the database's owner** itself, not a member of it
or a superuser: whoever runs a migration owns what it creates, and every
later migration must be able to change it. It refuses any other role, and
a server where `telflo_opamp_runtime` doesn't exist (run the bootstrap
first).
