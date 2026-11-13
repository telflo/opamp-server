-- An organization's limits, and the published versions' composite key.
--
--   * max_collectors: the most collectors the organization may have
--     enrolled, or null for no cap. Counted, not kept: an enrollment takes
--     the organization's row `for no key update`, then counts its
--     collectors and inserts only under the cap, in one transaction. No
--     counter to drift; lowering the cap below the count trims nothing (new
--     enrollments are refused until it drops).
--   * requests_per_minute: the organization's agent requests a minute, per
--     pod, or null for the server's default
--     (OPAMP_DEFAULT_REQUESTS_PER_MINUTE).
--   * publications gains unique (organization_id, id), the target of every
--     composite foreign key naming a published version: a group's
--     assignment, a collector's pin and what it was served can only name
--     the same organization's.
--   * Expand only: both columns are nullable with no default, so existing
--     organizations read "no cap" and "the default", and a server of the
--     previous release, which never names them, keeps working. The unique
--     key holds already: id is the primary key.
--   * telflo_opamp_server_runtime may update the two columns (limits are set
--     through the management API). The id and created_at stay immutable.
--
-- Down revokes the column grant and drops the unique key and both columns.

-- +goose Up
alter table opamp_server.organizations
  add column max_collectors      integer null,
  add column requests_per_minute integer null,
  add constraint ck_organizations_max_collectors_non_negative
    check (max_collectors is null or max_collectors >= 0),
  add constraint ck_organizations_requests_per_minute_positive
    check (requests_per_minute is null or requests_per_minute >= 1);

comment on column opamp_server.organizations.max_collectors is
  'the most collectors the organization may have enrolled, or null for no
   cap. Counted under the organization''s row lock at each enrollment, never
   kept as a counter. Lowering it below the count trims nothing: new
   enrollments are refused until the count drops.';
comment on column opamp_server.organizations.requests_per_minute is
  'the agent requests a minute the organization may make to each pod, or
   null for the server''s default, OPAMP_DEFAULT_REQUESTS_PER_MINUTE.
   Counted per pod: the fleet-wide limit is this times the number of pods.';
comment on constraint ck_organizations_max_collectors_non_negative on opamp_server.organizations is
  'a cap is zero or more (zero refuses every enrollment); null is no cap.';
comment on constraint ck_organizations_requests_per_minute_positive on opamp_server.organizations is
  'a limit is at least one request a minute; null is the server''s default.';

alter table opamp_server.publications
  add constraint uq_publications_organization_id_id unique (organization_id, id);

comment on constraint uq_publications_organization_id_id on opamp_server.publications is
  'the target of every composite foreign key naming a published version (a
   group''s assignment, a collector''s pin and what it was served), so a
   link can only reach the same organization''s. Its index also serves the
   in-use checks when a published version is deleted.';

grant update (max_collectors, requests_per_minute) on opamp_server.organizations to telflo_opamp_server_runtime;

-- +goose Down
revoke update (max_collectors, requests_per_minute) on opamp_server.organizations from telflo_opamp_server_runtime;
alter table opamp_server.publications drop constraint uq_publications_organization_id_id;
alter table opamp_server.organizations
  drop column requests_per_minute,
  drop column max_collectors;
