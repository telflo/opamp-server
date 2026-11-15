-- Collector groups.
--
-- Edited in place before any production install: the role, namespace,
-- replicas and volumes columns were removed, with their checks, comments
-- and the runtime role's column grants (a kubernetes-supervisor fleet's
-- install fields live in the group's annotations, 00011). A database
-- migrated before the edit (at version 11 with the four columns, or at a 12
-- that dropped them) is dropped and bootstrapped again (README.md,
-- "2. Migrate"). Every migration from here on is expand, then contract.
--
-- Every fleet has a Default group from its creation (the store makes it in
-- the fleet's transaction), and every collector is in a group of its own
-- fleet. A group's assigned published version is served to its collectors
-- that have no pin of their own.
--
--   * Its fleet by (organization_id, fleet_id) -> fleets, on delete
--     cascade; unique (organization_id, fleet_id, id) is the target of a
--     collector's group key, so a collector's group is always its fleet's.
--   * Its assigned published version by (organization_id,
--     assigned_publication_id) -> publications, NO ACTION: a published
--     version a group uses can't be deleted (23503, answered 409
--     publication_in_use), and only the organization's own can be assigned.
--   * Its organization by a direct (organization_id) -> organizations(id)
--     on delete cascade. Without it, deleting an organization fails
--     whenever the publications' cascade fires before the fleets' (the
--     older trigger does): the NO ACTION check above would find the group
--     not yet deleted. With every table's own cascade, all of them run
--     before any check, whatever the triggers' order.
--   * One Default group per fleet (a partial unique index). is_default and
--     fleet_id never change: telflo_opamp_server_runtime has no update on them.
--   * No column has a default; times from the server's injected clock.
--
-- Down drops the table and revokes every grant Up made.

-- +goose Up
create table opamp_server.collector_groups (
  id                       uuid         primary key,
  organization_id          uuid         not null,
  fleet_id                 uuid         not null,
  name                     text         not null,
  is_default               boolean      not null,
  assigned_publication_id  uuid         null,
  created_at               timestamptz  not null,
  updated_at               timestamptz  not null,
  constraint uq_collector_groups_organization_id_fleet_id_id unique (organization_id, fleet_id, id),
  constraint uq_collector_groups_organization_id_fleet_id_name unique (organization_id, fleet_id, name),
  constraint fk_collector_groups_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_collector_groups_fleet
    foreign key (organization_id, fleet_id)
    references opamp_server.fleets (organization_id, id)
    on delete cascade,
  constraint fk_collector_groups_assigned_publication
    foreign key (organization_id, assigned_publication_id)
    references opamp_server.publications (organization_id, id),
  constraint ck_collector_groups_name_trimmed_length
    check (name !~ '^[[:space:]]|[[:space:]]$' and char_length(name) between 1 and 80)
);

comment on table opamp_server.collector_groups is
  'a fleet''s collector groups: every fleet has a Default group, and every
   collector is in a group of its own fleet. The assigned published version
   is served to the group''s collectors without a pin.';
comment on column opamp_server.collector_groups.id is
  'the group''s id.';
comment on column opamp_server.collector_groups.organization_id is
  'the organization it belongs to: every query filters on it first, and
   every link to or from the group carries it.';
comment on column opamp_server.collector_groups.fleet_id is
  'the fleet it belongs to. Never changes: the runtime role has no update
   on it.';
comment on column opamp_server.collector_groups.name is
  'a display name, trimmed, 1 to 80 characters, unique within its fleet as
   written (case-sensitive). The Default group is named Default.
   Configuration text: never logged.';
comment on column opamp_server.collector_groups.is_default is
  'whether it is its fleet''s Default group, which is never deleted (one
   per fleet). Never changes: the runtime role has no update on it.';
comment on column opamp_server.collector_groups.assigned_publication_id is
  'the published version served to the group''s collectors that have no
   pin, or null for none. Always the organization''s own (a composite key),
   and while set the published version can''t be deleted.';
comment on column opamp_server.collector_groups.created_at is
  'when the server created it, by the injected clock, never now().';
comment on column opamp_server.collector_groups.updated_at is
  'when it last changed, by the injected clock; created_at until then.';

comment on constraint uq_collector_groups_organization_id_fleet_id_id on opamp_server.collector_groups is
  'the target of a collector''s group key, which carries its fleet, so a
   collector''s group is always its own fleet''s.';
comment on constraint uq_collector_groups_organization_id_fleet_id_name on opamp_server.collector_groups is
  'a group''s name is unique within its fleet, compared as written (409
   collector_group_name_taken). Its index also serves the fleet''s cascade
   and the organization''s.';
comment on constraint fk_collector_groups_organization on opamp_server.collector_groups is
  'the group goes with its organization, in the same pass as every other
   table''s cascade, so an organization''s delete never meets the NO ACTION
   check on its published versions.';
comment on constraint fk_collector_groups_fleet on opamp_server.collector_groups is
  'a group belongs to a fleet of its own organization, and goes with it.';
comment on constraint fk_collector_groups_assigned_publication on opamp_server.collector_groups is
  'an assigned published version is the organization''s own, and can''t be
   deleted while assigned (NO ACTION, answered 409 publication_in_use).';
comment on constraint ck_collector_groups_name_trimmed_length on opamp_server.collector_groups is
  'a name is stored trimmed, with no leading or trailing whitespace (a
   space, a tab, a line break), 1 to 80 characters.';
comment on constraint collector_groups_pkey on opamp_server.collector_groups is
  'a group''s id is unique on its own.';

create unique index uq_collector_groups_fleet_id_where_is_default
  on opamp_server.collector_groups (organization_id, fleet_id) where is_default;

comment on index opamp_server.uq_collector_groups_fleet_id_where_is_default is
  'at most one Default group per fleet. Its name leaves out the leading
   organization_id.';

create index idx_collector_groups_assigned_publication_id
  on opamp_server.collector_groups (organization_id, assigned_publication_id)
  where assigned_publication_id is not null;

comment on index opamp_server.idx_collector_groups_assigned_publication_id is
  'the groups a published version is assigned to: the NO ACTION check when
   one is deleted, and a configuration''s usage.';

grant select, insert, delete on opamp_server.collector_groups to telflo_opamp_server_runtime;
grant update (name, assigned_publication_id, updated_at)
  on opamp_server.collector_groups to telflo_opamp_server_runtime;

-- +goose Down
revoke update (name, assigned_publication_id, updated_at)
  on opamp_server.collector_groups from telflo_opamp_server_runtime;
revoke select, insert, delete on opamp_server.collector_groups from telflo_opamp_server_runtime;
drop table opamp_server.collector_groups;
