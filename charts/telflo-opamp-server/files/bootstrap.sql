-- The opamp-server's database and roles
-- (docs/decisions/0001-reached-only-through-its-api.md).
--
-- Run by a superuser, with psql 15 or later, before the first
-- `opamp-server migrate up`, with the two passwords in the environment:
--
--   OPAMP_OWNER_PASSWORD=… OPAMP_RUNTIME_PASSWORD=… \
--     psql -v ON_ERROR_STOP=1 [-v database=telflo_opamp] -f deploy/bootstrap.sql
--
-- (through `docker exec`, pass them with `-e OPAMP_OWNER_PASSWORD -e
-- OPAMP_RUNTIME_PASSWORD`, which copies each value from docker's own
-- environment, never with `-e NAME=value`).
--
-- The passwords come from the environment, read with psql's \getenv, and
-- never from `-v` arguments: a command line is visible to every user of the
-- machine in the process list (ps, /proc/<pid>/cmdline), while a process's
-- environment is readable only by its own user and root. A
-- `-v owner_password=…` or `-v runtime_password=…` is ignored. The
-- database's name is no secret, so it stays a `-v` variable.
--
-- Creating roles and a database needs rights neither the migrations nor the
-- server should hold, so this is a separate step, run by whoever administers
-- the Postgres server. It is idempotent and safe to rerun: Postgres's
-- docker-entrypoint-initdb.d runs only on an empty volume, and a server that
-- already holds other databases needs this run too. A second run changes
-- nothing but the two passwords, which it sets again from the environment.
--
-- The three roles:
--   telflo_opamp_owner    logs in; owns the database, the schema and every
--                         table; `opamp-server migrate` runs as it.
--   telflo_opamp_runtime  NOLOGIN; the migrations grant it exactly what the
--                         server needs, and nothing more.
--   telflo_opamp          logs in; `opamp-server serve` runs as it. A member
--                         of telflo_opamp_runtime, with no DDL rights.
-- Self-hosters may have the server log in under another name, as long as it
-- is a member of telflo_opamp_runtime.
--
-- The passwords reach Postgres in `create role` and `alter role` statements,
-- which take no bind parameters, and in the emptiness check below: a server
-- that sets log_statement to `ddl` or `all` writes them to its log, and any
-- server writes a statement that FAILS whole (log_min_error_statement), so
-- run this only as a superuser, where its password statements don't fail
-- (the README's managed Postgres section says what to do without one). psql
-- does not echo them unless run with -a or -e.

\set ON_ERROR_STOP on

-- Both passwords are required, and neither may be empty: a role with no
-- password can't log in over the network, and Postgres takes an empty one as
-- "no password". psql can't stop a script on its own, so a failing statement
-- does it (ON_ERROR_STOP makes psql exit with 3). The \unset first drops any
-- `-v` value, so only the environment counts.
\unset owner_password
\unset runtime_password
\getenv owner_password OPAMP_OWNER_PASSWORD
\getenv runtime_password OPAMP_RUNTIME_PASSWORD
\if :{?owner_password}
\else
  do $$ begin raise exception 'bootstrap.sql needs OPAMP_OWNER_PASSWORD in its environment'; end $$;
\endif
\if :{?runtime_password}
\else
  do $$ begin raise exception 'bootstrap.sql needs OPAMP_RUNTIME_PASSWORD in its environment'; end $$;
\endif
-- psql has no string comparison of its own, so the server compares (the
-- passwords reach it in the role statements below anyway). \gset leaves the
-- two answers, t or f, in psql variables for \if.
select :'owner_password' = '' as owner_password_empty,
       :'runtime_password' = '' as runtime_password_empty
\gset
\if :owner_password_empty
  do $$ begin raise exception 'bootstrap.sql needs OPAMP_OWNER_PASSWORD to be non-empty'; end $$;
\endif
\if :runtime_password_empty
  do $$ begin raise exception 'bootstrap.sql needs OPAMP_RUNTIME_PASSWORD to be non-empty'; end $$;
\endif

-- The database's name defaults to telflo_opamp.
\if :{?database}
\else
  \set database telflo_opamp
\endif

-- The roles. Created only when missing (roles belong to the whole server,
-- which other databases on it may share), with `\gexec` because psql doesn't
-- substitute variables inside a DO block. The attributes say what each role
-- may not do, rather than rely on Postgres's defaults.
select format(
         'create role telflo_opamp_owner login nosuperuser nocreatedb nocreaterole nobypassrls password %L',
         :'owner_password')
 where not exists (select 1 from pg_roles where rolname = 'telflo_opamp_owner')
\gexec

select 'create role telflo_opamp_runtime nologin nosuperuser nocreatedb nocreaterole nobypassrls'
 where not exists (select 1 from pg_roles where rolname = 'telflo_opamp_runtime')
\gexec

select format(
         'create role telflo_opamp login nosuperuser nocreatedb nocreaterole nobypassrls inherit password %L',
         :'runtime_password')
 where not exists (select 1 from pg_roles where rolname = 'telflo_opamp')
\gexec

-- Every run sets the passwords again, so rotating one is a rerun with a new
-- value. For the server's role it also states the attributes again: `serve`
-- refuses a superuser or a role with BYPASSRLS, CREATEDB or CREATEROLE, and
-- a rerun puts a role that was changed by hand back into this script's
-- shape.
alter role telflo_opamp_owner password :'owner_password';
alter role telflo_opamp login nosuperuser nocreatedb nocreaterole nobypassrls inherit
  password :'runtime_password';

-- The server's role gets the runtime role's privileges through membership,
-- inherited (stated: since Postgres 16 inheritance is set per grant). A
-- repeated grant only updates the existing one.
grant telflo_opamp_runtime to telflo_opamp with inherit true;

-- The database, owned by the owner role, so migrations can create the schema
-- in it without any superuser right. Its encoding is UTF8 whatever the
-- server's default (`serve` refuses any other), so it
-- is copied from template0, the one template a new encoding may come from.
-- Its locale is the server's default: Postgres refuses one that can't hold
-- UTF8 (a LATIN1 locale, say), and such a server needs the database made by
-- hand first, with `encoding 'UTF8' locale '<a UTF-8 locale, or C>'
-- template template0`, which this script then keeps.
select format('create database %I owner telflo_opamp_owner encoding %L template template0', :'database', 'UTF8')
 where not exists (select 1 from pg_database where datname = :'database')
\gexec

-- An existing database (created by hand, or by an older run) is handed to
-- the owner role too. A no-op when it already owns it.
alter database :"database" owner to telflo_opamp_owner;

-- No other role on the server can connect to this database
-- (docs/decisions/0001-reached-only-through-its-api.md): every role may
-- connect to a new database until PUBLIC's CONNECT is revoked, and then only
-- the two login roles here are let back in.
revoke connect on database :"database" from public;
grant connect on database :"database" to telflo_opamp_owner, telflo_opamp;

-- Nor may every role make temporary tables in it, which PUBLIC may in a new
-- database: the server needs none, and it runs no DDL. The owner
-- keeps the right through ownership.
revoke temporary on database :"database" from public;

-- The server's role finds the opamp-server's tables first. Every statement
-- in the code names the schema anyway (telflo_opamp.publications); this only
-- keeps a stray unqualified name from resolving into `public`.
alter role telflo_opamp in database :"database" set search_path = telflo_opamp;

-- The server's role's errors reach Postgres's own server log terse: without
-- DETAIL, HINT, QUERY or CONTEXT. A unique key's violation
-- quotes the value it refused in its DETAIL, and the values the
-- opamp-server keeps unique are configuration text the privacy rule keeps
-- out of every log: a fleet's label, a group's name, a
-- collector's name. The stores look for a taken name before they write,
-- so only two writes of one name racing each other reach the unique key;
-- this keeps that race's DETAIL out of the log too. What terse leaves is
-- the error's message and, under log_min_error_statement (error by
-- default), the statement, its values bound as $n placeholders, never
-- quoted. Only a superuser, or a role a superuser granted SET ON PARAMETER
-- log_error_verbosity, may set it, so the role can't set it back; it is set
-- for the role everywhere (like the role itself), and every run sets it
-- again. A self-hoster whose server logs in under another name sets it for
-- that role too. serve refuses to start unless it is terse.
alter role telflo_opamp set log_error_verbosity = 'terse';
