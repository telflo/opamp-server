-- Collectors (the collector_instances table) and their errors.
--
-- Edited in place before any production install: 00009_imports.sql was
-- deleted and this file renamed from 00010 and edited, without the import
-- columns, so a database migrated before (at version 10) is refused by
-- `serve` and must be dropped and bootstrapped again, never rolled back.
-- The reverse fails too: a build from before the edit can't run on a
-- database made after it (its `migrate up` fails:
-- `relation "collector_instances" already exists`); that database must be
-- dropped and bootstrapped again too. Its comments on a collector's group
-- were later edited in place too. Every migration from here on is expand,
-- then contract.
--
-- A collector is enrolled on its first poll, into a group of its token's
-- fleet, and polls every 30 seconds. What it is served is its pin (a
-- published version), else its group's assigned published version, else
-- nothing: it keeps running what it has.
--
--   * Every link a composite key carrying organization_id:
--     - its fleet, (organization_id, fleet_id) -> fleets, on delete cascade;
--     - its group, (organization_id, fleet_id, collector_group_id) ->
--       collector_groups, NO ACTION: its group is always its own fleet's (a
--       group's delete deletes its collectors first);
--     - its pin, NO ACTION: a pinned published version can't be deleted
--       (answered 409 publication_in_use);
--     - what it was served, on delete set null of that column alone
--       (Postgres 15's column list), so deleting a published version it was
--       served clears the pointer and keeps organization_id, fleet_id and
--       served_sha256;
--     - its token, (organization_id, fleet_id, enrollment_token_id) ->
--       enrollment_tokens, on delete set null of that column alone.
--   * Its organization by a direct (organization_id) -> organizations(id)
--     on delete cascade: without it, deleting an organization fails
--     whenever the publications' cascade fires first and the NO ACTION
--     check on a pin finds the collector not yet deleted.
--   * instance_uid and name are unique per organization; the first is
--     enrollment's arbiter (`insert ... on conflict do nothing`), and its
--     index leads the cap's count.
--   * The agent's writes are one update per poll, each part only when it
--     changed. last_seen_at is in no index, so a heartbeat is a HOT update;
--     fillfactor 80 leaves each page room for one. full_state_requested_at
--     holds "asked for its full state at most once per 3 minutes" across
--     replicas.
--   * Drift and "applied" come from hashes: served_sha256 against the
--     effective_sha256 the collector reports, with effective_status and the
--     two times.
--   * fleet_id, instance_uid and created_at never change:
--     telflo_opamp_server_runtime has no update on them (nor on id or
--     organization_id).
--   * collector_errors: a collector's FAILED reports, the newest 100 per
--     collector (the store trims at each insert) and 30 days (a cleanup
--     under an advisory lock); they go with their collector.
--   * No column has a default; times from the server's injected clock.
--
-- Down drops both tables and revokes every grant Up made.

-- +goose Up
create table opamp_server.collector_instances (
  id                          uuid         primary key,
  organization_id             uuid         not null,
  fleet_id                    uuid         not null,
  collector_group_id          uuid         not null,
  instance_uid                uuid         not null,
  name                        text         not null,
  pinned_publication_id       uuid         null,
  served_publication_id       uuid         null,
  served_sha256               text         null,
  served_at                   timestamptz  null,
  effective_sha256            text         null,
  effective_status            text         null,
  effective_error             text         null,
  effective_at                timestamptz  null,
  full_state_requested_at     timestamptz  null,
  reported_collector_version  text         null,
  reported_ip                 text         null,
  reported_host_ip            text         null,
  reported_attributes         jsonb        null,
  enrollment_token_id         uuid         null,
  last_seen_at                timestamptz  not null,
  created_at                  timestamptz  not null,
  updated_at                  timestamptz  not null,
  constraint uq_collector_instances_organization_id_id unique (organization_id, id),
  constraint uq_collector_instances_organization_id_instance_uid unique (organization_id, instance_uid),
  constraint uq_collector_instances_organization_id_name unique (organization_id, name),
  constraint fk_collector_instances_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_collector_instances_fleet
    foreign key (organization_id, fleet_id)
    references opamp_server.fleets (organization_id, id)
    on delete cascade,
  constraint fk_collector_instances_collector_group
    foreign key (organization_id, fleet_id, collector_group_id)
    references opamp_server.collector_groups (organization_id, fleet_id, id),
  constraint fk_collector_instances_pinned_publication
    foreign key (organization_id, pinned_publication_id)
    references opamp_server.publications (organization_id, id),
  constraint fk_collector_instances_served_publication
    foreign key (organization_id, served_publication_id)
    references opamp_server.publications (organization_id, id)
    on delete set null (served_publication_id),
  constraint fk_collector_instances_enrollment_token
    foreign key (organization_id, fleet_id, enrollment_token_id)
    references opamp_server.enrollment_tokens (organization_id, fleet_id, id)
    on delete set null (enrollment_token_id),
  constraint ck_collector_instances_name_trimmed_length
    check (name !~ '^[[:space:]]|[[:space:]]$' and char_length(name) between 1 and 200),
  constraint ck_collector_instances_served_sha256_hex
    check (served_sha256 is null or served_sha256 ~ '^[0-9a-f]{64}$'),
  constraint ck_collector_instances_effective_sha256_hex
    check (effective_sha256 is null or effective_sha256 ~ '^[0-9a-f]{64}$'),
  constraint ck_collector_instances_effective_status
    check (effective_status is null or effective_status in ('applied', 'applying', 'failed')),
  constraint ck_collector_instances_effective_error_size
    check (effective_error is null or octet_length(effective_error) between 1 and 8192),
  constraint ck_collector_instances_reported_collector_version_length
    check (reported_collector_version is null or char_length(reported_collector_version) between 1 and 200),
  constraint ck_collector_instances_reported_ip_length
    check (reported_ip is null or char_length(reported_ip) between 1 and 100),
  constraint ck_collector_instances_reported_host_ip_length
    check (reported_host_ip is null or char_length(reported_host_ip) between 1 and 100),
  constraint ck_collector_instances_reported_attributes_bounded
    check (reported_attributes is null
           or (jsonb_typeof(reported_attributes) = 'object'
               and octet_length(reported_attributes::text) <= 16384))
) with (fillfactor = 80);

comment on table opamp_server.collector_instances is
  'collectors: enrolled on their first poll into a group of their token''s
   fleet, polled every 30 seconds. What each is served, reports and was last
   seen. Fillfactor 80 keeps the heartbeat a HOT update.';
comment on column opamp_server.collector_instances.id is
  'the row''s id, which the management routes name.';
comment on column opamp_server.collector_instances.organization_id is
  'the organization it belongs to: every query filters on it first, and
   every link carries it.';
comment on column opamp_server.collector_instances.fleet_id is
  'its fleet, its token''s when it enrolled. Never changes: the runtime role
   has no update on it.';
comment on column opamp_server.collector_instances.collector_group_id is
  'its group, always of its own fleet (a composite key): the one it named
   when it enrolled (one naming none is refused), until an operator moves
   it.';
comment on column opamp_server.collector_instances.instance_uid is
  'the OpAMP instance uid it reports, unique per organization: enrollment''s
   arbiter. Never changes: the runtime role has no update on it.';
comment on column opamp_server.collector_instances.name is
  'its ServerName when it enrolled, else a petname; trimmed, 1 to 200
   characters, unique per organization as written. Configuration text: never
   logged.';
comment on column opamp_server.collector_instances.pinned_publication_id is
  'a published version pinned on it, which wins over its group''s
   assignment, or null. The organization''s own, and while pinned it
   can''t be deleted.';
comment on column opamp_server.collector_instances.served_publication_id is
  'the published version it was last served, or null; set null alone when
   that published version is deleted.';
comment on column opamp_server.collector_instances.served_sha256 is
  'the SHA-256 of the YAML last served or recorded, lowercase hex: a push
   happens when what resolves hashes otherwise. Kept when the published
   version goes.';
comment on column opamp_server.collector_instances.served_at is
  'when it was last served or recorded, by the injected clock: drift''s "no
   report since it was served".';
comment on column opamp_server.collector_instances.effective_sha256 is
  'the configuration hash its last RemoteConfigStatus reported, lowercase
   hex: in sync when it equals served_sha256 and applied.';
comment on column opamp_server.collector_instances.effective_status is
  'applied, applying or failed, as it last reported (an APPLIED with an
   unhealthy Health counts as failed), or null before any report.';
comment on column opamp_server.collector_instances.effective_error is
  'its last FAILED report''s message, 1 byte to 8 KiB, or null.
   Configuration text: never logged.';
comment on column opamp_server.collector_instances.effective_at is
  'when its status was last reported, by the injected clock.';
comment on column opamp_server.collector_instances.full_state_requested_at is
  'when it was last asked for its full state with nothing resolved, by the
   injected clock: at most once per 3 minutes, whichever replica it polls.';
comment on column opamp_server.collector_instances.reported_collector_version is
  'the collector version it reports (1 to 200 characters), or null.';
comment on column opamp_server.collector_instances.reported_ip is
  'the client address its polls come from (1 to 100 characters), for
   display only, never trusted.';
comment on column opamp_server.collector_instances.reported_host_ip is
  'the host.ip its agent description reports (1 to 100 characters), or
   null.';
comment on column opamp_server.collector_instances.reported_attributes is
  'its agent description''s attributes, a JSON object of at most 16 KiB, or
   null. Their values are configuration text: never logged.';
comment on column opamp_server.collector_instances.enrollment_token_id is
  'the token of its fleet it last authenticated with, or null; set null
   alone if that token goes.';
comment on column opamp_server.collector_instances.last_seen_at is
  'its last heartbeat, by the injected clock, written at most every 15
   seconds: liveness. In no index, so the heartbeat is a HOT update.';
comment on column opamp_server.collector_instances.created_at is
  'when it enrolled, by the injected clock, never now(). Never changes.';
comment on column opamp_server.collector_instances.updated_at is
  'when an operator last changed it (rename, move, pin), by the injected
   clock; created_at until then.';

comment on constraint uq_collector_instances_organization_id_id on opamp_server.collector_instances is
  'the target of an error row''s collector key, so an error is always its
   own organization''s collector''s.';
comment on constraint uq_collector_instances_organization_id_instance_uid on opamp_server.collector_instances is
  'one collector per instance uid per organization: enrollment''s arbiter.
   Its index serves the poll''s lookup, the cap''s count (index-only) and
   the organization''s cascade.';
comment on constraint uq_collector_instances_organization_id_name on opamp_server.collector_instances is
  'a collector''s name is unique within its organization, compared as
   written (409 collector_name_taken; enrollment picks a petname instead).';
comment on constraint fk_collector_instances_organization on opamp_server.collector_instances is
  'the collector goes with its organization, in the same pass as every other
   table''s cascade, so an organization''s delete never meets the NO ACTION
   check on a pinned published version.';
comment on constraint fk_collector_instances_fleet on opamp_server.collector_instances is
  'a collector belongs to a fleet of its own organization, and goes with
   it.';
comment on constraint fk_collector_instances_collector_group on opamp_server.collector_instances is
  'a collector''s group is a group of its own fleet. NO ACTION: a group''s
   delete deletes its collectors first.';
comment on constraint fk_collector_instances_pinned_publication on opamp_server.collector_instances is
  'a pinned published version is the organization''s own, and can''t be
   deleted while pinned (NO ACTION, answered 409 publication_in_use).';
comment on constraint fk_collector_instances_served_publication on opamp_server.collector_instances is
  'what it was served is the organization''s own; deleting it sets
   served_publication_id alone to null, keeping organization_id (the
   column list).';
comment on constraint fk_collector_instances_enrollment_token on opamp_server.collector_instances is
  'its token is one of its own fleet''s; deleting it sets
   enrollment_token_id alone to null, keeping organization_id and
   fleet_id.';
comment on constraint ck_collector_instances_name_trimmed_length on opamp_server.collector_instances is
  'a name is stored trimmed, with no leading or trailing whitespace (a
   space, a tab, a line break), 1 to 200 characters.';
comment on constraint ck_collector_instances_served_sha256_hex on opamp_server.collector_instances is
  'a SHA-256 in lowercase hex, or null.';
comment on constraint ck_collector_instances_effective_sha256_hex on opamp_server.collector_instances is
  'a SHA-256 in lowercase hex, or null.';
comment on constraint ck_collector_instances_effective_status on opamp_server.collector_instances is
  'applied, applying or failed, or null.';
comment on constraint ck_collector_instances_effective_error_size on opamp_server.collector_instances is
  'an error message is 1 byte to 8 KiB, or null.';
comment on constraint ck_collector_instances_reported_collector_version_length on opamp_server.collector_instances is
  'a reported version is 1 to 200 characters, or null.';
comment on constraint ck_collector_instances_reported_ip_length on opamp_server.collector_instances is
  'an address is 1 to 100 characters, or null.';
comment on constraint ck_collector_instances_reported_host_ip_length on opamp_server.collector_instances is
  'an address is 1 to 100 characters, or null.';
comment on constraint ck_collector_instances_reported_attributes_bounded on opamp_server.collector_instances is
  'reported attributes are a JSON object of at most 16 KiB, or null.';
comment on constraint collector_instances_pkey on opamp_server.collector_instances is
  'a collector''s id is unique on its own.';

create index idx_collector_instances_fleet_id_created_at
  on opamp_server.collector_instances (organization_id, fleet_id, created_at desc, id desc);

comment on index opamp_server.idx_collector_instances_fleet_id_created_at is
  'a fleet''s collectors newest first (the list''s default sort), the
   fleet''s summaries and its cascade. Its name leaves out the leading
   organization_id; id breaks ties.';

create index idx_collector_instances_fleet_id_name
  on opamp_server.collector_instances (organization_id, fleet_id, name);

comment on index opamp_server.idx_collector_instances_fleet_id_name is
  'a fleet''s collectors sorted by name.';

create index idx_collector_instances_fleet_id_collector_group_id
  on opamp_server.collector_instances (organization_id, fleet_id, collector_group_id);

comment on index opamp_server.idx_collector_instances_fleet_id_collector_group_id is
  'a group''s collectors: the list''s groupId filter, a group''s delete
   deleting them, and the NO ACTION check on a group''s delete.';

create index idx_collector_instances_pinned_publication_id
  on opamp_server.collector_instances (organization_id, pinned_publication_id)
  where pinned_publication_id is not null;

comment on index opamp_server.idx_collector_instances_pinned_publication_id is
  'the collectors a published version is pinned on: the NO ACTION check
   when one is deleted, and a configuration''s usage.';

create index idx_collector_instances_served_publication_id
  on opamp_server.collector_instances (organization_id, served_publication_id)
  where served_publication_id is not null;

comment on index opamp_server.idx_collector_instances_served_publication_id is
  'the collectors a published version was served to: the set-null action
   when one is deleted.';

create index idx_collector_instances_fleet_id_enrollment_token_id
  on opamp_server.collector_instances (organization_id, fleet_id, enrollment_token_id)
  where enrollment_token_id is not null;

comment on index opamp_server.idx_collector_instances_fleet_id_enrollment_token_id is
  'the collectors on each token (the token list''s counts), and the set-null
   action.';

grant select, insert, delete on opamp_server.collector_instances to telflo_opamp_server_runtime;
grant update (name, collector_group_id, pinned_publication_id,
              served_publication_id, served_sha256, served_at,
              effective_sha256, effective_status, effective_error, effective_at,
              full_state_requested_at, reported_collector_version, reported_ip,
              reported_host_ip, reported_attributes, enrollment_token_id,
              last_seen_at, updated_at)
  on opamp_server.collector_instances to telflo_opamp_server_runtime;

create table opamp_server.collector_errors (
  id                     uuid         primary key,
  organization_id        uuid         not null,
  collector_instance_id  uuid         not null,
  reported_at            timestamptz  not null,
  config_sha256          text         null,
  message                text         not null,
  constraint fk_collector_errors_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_collector_errors_collector_instance
    foreign key (organization_id, collector_instance_id)
    references opamp_server.collector_instances (organization_id, id)
    on delete cascade,
  constraint ck_collector_errors_config_sha256_hex
    check (config_sha256 is null or config_sha256 ~ '^[0-9a-f]{64}$'),
  constraint ck_collector_errors_message_size
    check (octet_length(message) between 1 and 8192)
);

comment on table opamp_server.collector_errors is
  'a collector''s error history: one row per FAILED report, the newest 100
   per collector, kept 30 days. Its messages are configuration text: never
   logged.';
comment on column opamp_server.collector_errors.id is
  'the row''s id.';
comment on column opamp_server.collector_errors.organization_id is
  'the organization it belongs to: every query filters on it first.';
comment on column opamp_server.collector_errors.collector_instance_id is
  'the collector that reported it, of the same organization; the row goes
   with it.';
comment on column opamp_server.collector_errors.reported_at is
  'when the server received it, by the injected clock, never now(): the
   30-day cleanup''s key.';
comment on column opamp_server.collector_errors.config_sha256 is
  'the configuration hash the collector failed to apply, lowercase hex, or
   null when it named none.';
comment on column opamp_server.collector_errors.message is
  'the error the collector reported, 1 byte to 8 KiB.';

comment on constraint fk_collector_errors_organization on opamp_server.collector_errors is
  'the row goes with its organization, in the same pass as every other
   table''s cascade.';
comment on constraint fk_collector_errors_collector_instance on opamp_server.collector_errors is
  'an error row belongs to a collector of its own organization, and goes
   with it.';
comment on constraint ck_collector_errors_config_sha256_hex on opamp_server.collector_errors is
  'a SHA-256 in lowercase hex, or null.';
comment on constraint ck_collector_errors_message_size on opamp_server.collector_errors is
  'a message is 1 byte to 8 KiB.';
comment on constraint collector_errors_pkey on opamp_server.collector_errors is
  'an error row''s id is unique on its own.';

create index idx_collector_errors_collector_instance_id_reported_at
  on opamp_server.collector_errors (organization_id, collector_instance_id, reported_at desc, id desc);

comment on index opamp_server.idx_collector_errors_collector_instance_id_reported_at is
  'a collector''s history newest first, paged; the trim to its newest 100,
   and the collector''s cascade. id breaks ties.';

create index idx_collector_errors_reported_at
  on opamp_server.collector_errors (organization_id, reported_at);

comment on index opamp_server.idx_collector_errors_reported_at is
  'the 30-day cleanup, by a skip scan over the organizations.';

grant select, insert, delete on opamp_server.collector_errors to telflo_opamp_server_runtime;

-- +goose Down
revoke select, insert, delete on opamp_server.collector_errors from telflo_opamp_server_runtime;
drop table opamp_server.collector_errors;
revoke update (name, collector_group_id, pinned_publication_id,
               served_publication_id, served_sha256, served_at,
               effective_sha256, effective_status, effective_error, effective_at,
               full_state_requested_at, reported_collector_version, reported_ip,
               reported_host_ip, reported_attributes, enrollment_token_id,
               last_seen_at, updated_at)
  on opamp_server.collector_instances from telflo_opamp_server_runtime;
revoke select, insert, delete on opamp_server.collector_instances from telflo_opamp_server_runtime;
drop table opamp_server.collector_instances;
