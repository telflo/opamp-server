-- A new default token after a revoke.
--
-- 00008's partial unique index allowed one default token per fleet ever: a
-- revoked one's row (never deleted but with its fleet) kept any other from
-- being made, so a fleet whose default token leaked could only be
-- re-created. This narrows it to the fleet's VALID default token: a default
-- token never expires (ck_enrollment_tokens_default_never_expires), so
-- valid is not revoked. A revoked default token stays, with its events, and
-- a new one may be issued beside it (store/enrollmenttokens IssueDefault).
--
-- Expand only: the index is looser, so a server of the previous release
-- (which never issues a second default token) keeps working while this goes
-- in. No grant changes.
--
-- First it refuses a database migrated before 00009's edit in place, as
-- 00011 does: one left at the old 9 (the imports, a migration since
-- deleted) finds this one pending, and is refused here, untouched, rather
-- than narrowed and then refused by 00011. Its opamp_server.imports table,
-- which no current migration makes, marks it.
--
-- Down restores 00008's index. It fails (23505, naming the index) once any
-- fleet holds a revoked default token and a new one: an older server's
-- model can't hold that fleet, and nothing here deletes a token or its
-- audit trail to make it fit.

-- +goose Up
-- +goose StatementBegin
do $$
begin
  if to_regclass('opamp_server.imports') is not null then
    raise exception 'this database holds the old imports table (opamp_server.imports): recreate it (see the opamp-server README, "A database holding the old imports table")'
      using errcode = 'object_not_in_prerequisite_state';
  end if;
end
$$;
-- +goose StatementEnd

drop index opamp_server.uq_enrollment_tokens_fleet_id_where_kind_default;

create unique index uq_enrollment_tokens_fleet_id_where_default_not_revoked
  on opamp_server.enrollment_tokens (organization_id, fleet_id)
  where kind = 'default' and revoked_at is null;

comment on index opamp_server.uq_enrollment_tokens_fleet_id_where_default_not_revoked is
  'at most one valid default token per fleet: a default token never expires,
   so valid is not revoked, and a revoked one may be followed by a new one.
   A second issue racing the first loses here. Its name leaves out the
   leading organization_id.';

-- +goose Down
drop index opamp_server.uq_enrollment_tokens_fleet_id_where_default_not_revoked;

create unique index uq_enrollment_tokens_fleet_id_where_kind_default
  on opamp_server.enrollment_tokens (organization_id, fleet_id) where kind = 'default';

comment on index opamp_server.uq_enrollment_tokens_fleet_id_where_kind_default is
  'at most one default token per fleet. Its name leaves out the leading
   organization_id.';
