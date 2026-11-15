-- Organization variables and variable groups.
--
-- The opamp-server owns an organization's variables: each a name, a value
-- and a description, filled into a published version's ${org:NAME}
-- references at poll. A variable group holds overrides only; a collector
-- group and a collector's pin may each use one, and each variable resolves
-- from the pin's variable group, else the collector group's, else the
-- organization's value.
--
--   * organizations.variables_revision is the organization's counter: +1 in
--     the transaction of every write that changes filled-in text, and its
--     values are the change stamps. A stamp is written only on the row that
--     changed: a variable's value, an override (its Reset keeps the row
--     with no value, a reset row), a collector group's or a pin's
--     variable-group setting, a collector moved between groups whose
--     variable groups differ. served_variables_stamp is each collector's
--     marker: the newest stamp it was served with.
--   * organizations.generation is a random uuid the store writes when it
--     registers an organization: an organization deleted and registered
--     again under the same id starts its counter at 0 again, so the poll's
--     per-pod variables cache keys its entries by the generation too, and
--     never serves one incarnation's values to the next.
--   * publication_variable_names holds the names each published version
--     references, written at publish only: the in-use guards (409
--     variable_in_use) and the undefined check (422 variables_undefined)
--     read it.
--   * Every link a composite key carrying organization_id, and every new
--     table its own (organization_id) -> organizations(id) on delete
--     cascade. The bindings are NO ACTION: a variable group in use can't be
--     deleted (23503, answered 409 variable_group_in_use). A null binding
--     skips its key (MATCH SIMPLE).
--   * Adds only: tables, nullable columns and the counter column. No
--     release before this migration runs in production, so nothing here is
--     for a previous release's server. The counter alone has a default, 0,
--     because the existing organizations' rows need a value when the column
--     is added; the store still names it on every insert. The generation
--     has none: the rows that exist when it is added get a random uuid each
--     from a default dropped in the same migration, and the store writes it
--     on every insert. Every other time and value is the server's (times
--     from its injected clock).
--   * No guard against a database migrated before 00009's edit in place, as
--     00010 and 00011 have: such a database finds 00010 or 00011 pending
--     first, and each refuses it in its own transaction, so 00012 is never
--     pending on one.
--
-- Down revokes every grant Up made and drops what Up made, in reverse order.

-- +goose Up

-- 1. The organization's counter.
alter table opamp_server.organizations
  add column variables_revision bigint not null default 0,
  add constraint ck_organizations_variables_revision_non_negative
    check (variables_revision >= 0);

comment on column opamp_server.organizations.variables_revision is
  'the counter: +1 in the transaction of every write that changes filled-in
   text, and its values are the stamps. Never a time, so replicas'' clocks
   can''t misorder it. Its default 0 is only for the rows that existed when
   it was added: the store names it on every insert.';
comment on constraint ck_organizations_variables_revision_non_negative on opamp_server.organizations is
  'the counter starts at 0 and only grows.';

grant update (variables_revision) on opamp_server.organizations to telflo_opamp_server_runtime;

-- The organization's generation. Its select and insert come with the
-- table's grants (00002); no update: a generation never changes.
alter table opamp_server.organizations
  add column generation uuid not null default gen_random_uuid();
alter table opamp_server.organizations
  alter column generation drop default;

comment on column opamp_server.organizations.generation is
  'a random uuid the store writes when it registers the organization, never
   changed: an organization deleted and registered again under its id has
   another, so the poll''s variables cache, keyed by it with the counter,
   never serves a previous incarnation''s values. No default: the rows that
   existed when it was added each got a random one.';

-- 2. The organization's variables.
create table opamp_server.variables (
  id                uuid         primary key,
  organization_id   uuid         not null,
  name              text         not null,
  description       text         null,
  value             text         not null,
  changed_by_id     uuid         null,
  changed_by_email  text         null,
  changed_at        timestamptz  not null,
  stamp             bigint       not null,
  constraint uq_variables_organization_id_id unique (organization_id, id),
  constraint uq_variables_organization_id_name unique (organization_id, name),
  constraint fk_variables_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint ck_variables_name_format
    check (name ~ '^[A-Za-z_][A-Za-z0-9_]*$' and char_length(name) <= 128),
  constraint ck_variables_value_single_line
    check (char_length(value) <= 1024 and value !~ '[\x01-\x1f\x7f-\x9f\u2028\u2029]'),
  constraint ck_variables_description_trimmed_length
    check (description is null
           or (description !~ '^[[:space:]]|[[:space:]]$' and char_length(description) between 1 and 1000)),
  constraint ck_variables_changed_by_both_or_neither
    check ((changed_by_id is null) = (changed_by_email is null)),
  constraint ck_variables_changed_by_email_length
    check (changed_by_email is null or char_length(changed_by_email) between 1 and 320),
  constraint ck_variables_stamp_positive
    check (stamp >= 1)
);

comment on table opamp_server.variables is
  'an organization''s variables: each filled into a published version''s
   ${org:NAME} references at poll, unless a variable group overrides it. At
   most 150 per organization (the store counts them). Values are
   configuration text, not secrets: never logged.';
comment on column opamp_server.variables.id is
  'the variable''s id; the caller may choose it, which makes a create
   idempotent.';
comment on column opamp_server.variables.organization_id is
  'the organization it belongs to: every query filters on it first, and
   every link to it carries it.';
comment on column opamp_server.variables.name is
  'the reference''s NAME, ${org:NAME}: the renderer''s grammar, ASCII, at
   most 128 characters, unique within its organization as written
   (case-sensitive). Configuration text: never logged.';
comment on column opamp_server.variables.description is
  'an optional description, trimmed, 1 to 1,000 characters, or null.
   Configuration text: never logged.';
comment on column opamp_server.variables.value is
  'the organization''s value: 0 to 1,024 characters on one line, filled in
   with each $ written as $$. Configuration text: never logged.';
comment on column opamp_server.variables.changed_by_id is
  'who last changed it, the platform''s person id as the caller passed it,
   never a foreign key. Null with changed_by_email, for a change no person
   made.';
comment on column opamp_server.variables.changed_by_email is
  'their email then, a snapshot, 1 to 320 characters. Null exactly when
   changed_by_id is.';
comment on column opamp_server.variables.changed_at is
  'when it last changed (any change, its description included), by the
   injected clock, never now().';
comment on column opamp_server.variables.stamp is
  'the organization''s counter value of its last create, value change or
   rename: a collector whose text uses its value reads Pending until served
   past it.';

comment on constraint uq_variables_organization_id_id on opamp_server.variables is
  'the target of an override''s variable key, which carries the
   organization, so an override is always of its own organization''s
   variable.';
comment on constraint uq_variables_organization_id_name on opamp_server.variables is
  'a name is unique within its organization, compared as written (409
   variable_name_taken). Its index also serves the fill-in''s and the
   guards'' lookups by name, and the organization''s cascade.';
comment on constraint fk_variables_organization on opamp_server.variables is
  'a variable goes with its organization, in the same pass as every other
   table''s cascade.';
comment on constraint ck_variables_name_format on opamp_server.variables is
  'a name is the renderer''s grammar, [A-Za-z_][A-Za-z0-9_]*, in ASCII, at
   most 128 characters.';
comment on constraint ck_variables_value_single_line on opamp_server.variables is
  'a value is at most 1,024 characters on one line: no C0 control (text
   can''t hold U+0000), DEL, C1 control, U+2028 or U+2029. Empty is allowed.
   Written as escapes, never the characters.';
comment on constraint ck_variables_description_trimmed_length on opamp_server.variables is
  'a description is null, or stored trimmed, with no leading or trailing
   whitespace, 1 to 1,000 characters.';
comment on constraint ck_variables_changed_by_both_or_neither on opamp_server.variables is
  'who changed it: the id and email are both known or both null.';
comment on constraint ck_variables_changed_by_email_length on opamp_server.variables is
  'an email is 1 to 320 characters.';
comment on constraint ck_variables_stamp_positive on opamp_server.variables is
  'a stamp is a counter value after its bump, so at least 1.';
comment on constraint variables_pkey on opamp_server.variables is
  'a variable''s id is unique on its own.';

grant select, insert, delete on opamp_server.variables to telflo_opamp_server_runtime;
grant update (name, description, value, changed_by_id, changed_by_email, changed_at, stamp)
  on opamp_server.variables to telflo_opamp_server_runtime;

-- 3. The organization's variable groups.
create table opamp_server.variable_groups (
  id                uuid         primary key,
  organization_id   uuid         not null,
  name              text         not null,
  description       text         null,
  changed_by_id     uuid         null,
  changed_by_email  text         null,
  changed_at        timestamptz  not null,
  constraint uq_variable_groups_organization_id_id unique (organization_id, id),
  constraint uq_variable_groups_organization_id_name unique (organization_id, name),
  constraint fk_variable_groups_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint ck_variable_groups_name_trimmed_length
    check (name !~ '^[[:space:]]|[[:space:]]$' and char_length(name) between 1 and 200),
  constraint ck_variable_groups_description_trimmed_length
    check (description is null
           or (description !~ '^[[:space:]]|[[:space:]]$' and char_length(description) between 1 and 1000)),
  constraint ck_variable_groups_changed_by_both_or_neither
    check ((changed_by_id is null) = (changed_by_email is null)),
  constraint ck_variable_groups_changed_by_email_length
    check (changed_by_email is null or char_length(changed_by_email) between 1 and 320)
);

comment on table opamp_server.variable_groups is
  'an organization''s variable groups: each holds overrides only, and a
   collector group or a collector''s pin may use one. At most 100 per
   organization (the store counts them). No stamp: nothing on its row feeds
   a collector''s status.';
comment on column opamp_server.variable_groups.id is
  'the variable group''s id; the caller may choose it, which makes a create
   idempotent.';
comment on column opamp_server.variable_groups.organization_id is
  'the organization it belongs to: every query filters on it first, and
   every link to it carries it.';
comment on column opamp_server.variable_groups.name is
  'a display name, trimmed, 1 to 200 characters, unique within its
   organization as written. Configuration text: never logged.';
comment on column opamp_server.variable_groups.description is
  'an optional description, trimmed, 1 to 1,000 characters, or null.
   Configuration text: never logged.';
comment on column opamp_server.variable_groups.changed_by_id is
  'who last changed it or an override in it, as the caller passed it, never
   a foreign key. Null with changed_by_email, for a change no person made.
   For display only.';
comment on column opamp_server.variable_groups.changed_by_email is
  'their email then, a snapshot, 1 to 320 characters. Null exactly when
   changed_by_id is.';
comment on column opamp_server.variable_groups.changed_at is
  'when it or an override in it last changed, by the injected clock, never
   now(). For display only.';

comment on constraint uq_variable_groups_organization_id_id on opamp_server.variable_groups is
  'the target of an override''s, a collector group''s and a pin''s
   variable-group keys, which carry the organization, so each names only its
   own organization''s variable group.';
comment on constraint uq_variable_groups_organization_id_name on opamp_server.variable_groups is
  'a name is unique within its organization, compared as written (409
   variable_group_name_taken). Its index also serves the organization''s
   cascade.';
comment on constraint fk_variable_groups_organization on opamp_server.variable_groups is
  'a variable group goes with its organization, in the same pass as every
   other table''s cascade, so an organization''s delete never meets the NO
   ACTION checks of the bindings.';
comment on constraint ck_variable_groups_name_trimmed_length on opamp_server.variable_groups is
  'a name is stored trimmed, with no leading or trailing whitespace (a
   space, a tab, a line break), 1 to 200 characters.';
comment on constraint ck_variable_groups_description_trimmed_length on opamp_server.variable_groups is
  'a description is null, or stored trimmed, with no leading or trailing
   whitespace, 1 to 1,000 characters.';
comment on constraint ck_variable_groups_changed_by_both_or_neither on opamp_server.variable_groups is
  'who changed it: the id and email are both known or both null.';
comment on constraint ck_variable_groups_changed_by_email_length on opamp_server.variable_groups is
  'an email is 1 to 320 characters.';
comment on constraint variable_groups_pkey on opamp_server.variable_groups is
  'a variable group''s id is unique on its own.';

grant select, insert, delete on opamp_server.variable_groups to telflo_opamp_server_runtime;
grant update (name, description, changed_by_id, changed_by_email, changed_at)
  on opamp_server.variable_groups to telflo_opamp_server_runtime;

-- 4. A variable group's overrides.
create table opamp_server.variable_group_overrides (
  organization_id    uuid         not null,
  variable_group_id  uuid         not null,
  variable_id        uuid         not null,
  value              text         null,
  changed_by_id      uuid         null,
  changed_by_email   text         null,
  changed_at         timestamptz  not null,
  stamp              bigint       not null,
  constraint variable_group_overrides_pkey
    primary key (organization_id, variable_group_id, variable_id),
  constraint fk_variable_group_overrides_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_variable_group_overrides_variable_group
    foreign key (organization_id, variable_group_id)
    references opamp_server.variable_groups (organization_id, id)
    on delete cascade,
  constraint fk_variable_group_overrides_variable
    foreign key (organization_id, variable_id)
    references opamp_server.variables (organization_id, id)
    on delete cascade,
  constraint ck_variable_group_overrides_value_single_line
    check (value is null
           or (char_length(value) <= 1024 and value !~ '[\x01-\x1f\x7f-\x9f\u2028\u2029]')),
  constraint ck_variable_group_overrides_changed_by_both_or_neither
    check ((changed_by_id is null) = (changed_by_email is null)),
  constraint ck_variable_group_overrides_changed_by_email_length
    check (changed_by_email is null or char_length(changed_by_email) between 1 and 320),
  constraint ck_variable_group_overrides_stamp_positive
    check (stamp >= 1)
);

comment on table opamp_server.variable_group_overrides is
  'a variable group''s overrides: at most one row per variable group and
   variable. A row with a value overrides the organization''s value for the
   collectors that use the group. No row means the organization''s value.';
comment on column opamp_server.variable_group_overrides.organization_id is
  'the organization it belongs to: every query filters on it first, and both
   its links carry it.';
comment on column opamp_server.variable_group_overrides.variable_group_id is
  'the variable group it is in, the organization''s own. The row goes with
   it.';
comment on column opamp_server.variable_group_overrides.variable_id is
  'the variable it overrides, the organization''s own. The row goes with
   it.';
comment on column opamp_server.variable_group_overrides.value is
  'the override''s value, as a variable''s value is bounded (one line, 0 to
   1,024 characters), or null for a reset row: an override''s Reset keeps
   its row with no value and the Reset''s stamp. A reset row is ignored when
   filling in, not counted among the group''s overrides, and reused by a
   later override of that variable, so reset rows are bounded as overrides
   are. Configuration text: never logged.';
comment on column opamp_server.variable_group_overrides.changed_by_id is
  'who last set, changed or Reset it, as the caller passed it, never a
   foreign key. Null with changed_by_email, for a change no person made.';
comment on column opamp_server.variable_group_overrides.changed_by_email is
  'their email then, a snapshot, 1 to 320 characters. Null exactly when
   changed_by_id is.';
comment on column opamp_server.variable_group_overrides.changed_at is
  'when it was last set, changed or Reset, by the injected clock, never
   now().';
comment on column opamp_server.variable_group_overrides.stamp is
  'the organization''s counter value of its last set, change or Reset: the
   row that changed is stamped, never the variable group.';

comment on constraint variable_group_overrides_pkey on opamp_server.variable_group_overrides is
  'one row per variable group and variable, led by organization_id. Its
   index serves the group''s cascade and the fill-in''s join.';
comment on constraint fk_variable_group_overrides_organization on opamp_server.variable_group_overrides is
  'an override goes with its organization, in the same pass as every other
   table''s cascade.';
comment on constraint fk_variable_group_overrides_variable_group on opamp_server.variable_group_overrides is
  'an override is in a variable group of its own organization, and goes with
   it.';
comment on constraint fk_variable_group_overrides_variable on opamp_server.variable_group_overrides is
  'an override is of a variable of its own organization, and goes with it.';
comment on constraint ck_variable_group_overrides_value_single_line on opamp_server.variable_group_overrides is
  'a value is bounded as a variable''s, or null for a reset row. The null
   guard is redundant (a check passes on null) but says so for the reader.';
comment on constraint ck_variable_group_overrides_changed_by_both_or_neither on opamp_server.variable_group_overrides is
  'who changed it: the id and email are both known or both null.';
comment on constraint ck_variable_group_overrides_changed_by_email_length on opamp_server.variable_group_overrides is
  'an email is 1 to 320 characters.';
comment on constraint ck_variable_group_overrides_stamp_positive on opamp_server.variable_group_overrides is
  'a stamp is a counter value after its bump, so at least 1.';

create index idx_variable_group_overrides_variable_id
  on opamp_server.variable_group_overrides (organization_id, variable_id);

comment on index opamp_server.idx_variable_group_overrides_variable_id is
  'a variable''s overrides: its cascade, and the fill-in''s join from each
   variable to its overrides. Its name leaves out the leading
   organization_id.';

grant select, insert, delete on opamp_server.variable_group_overrides to telflo_opamp_server_runtime;
grant update (value, changed_by_id, changed_by_email, changed_at, stamp)
  on opamp_server.variable_group_overrides to telflo_opamp_server_runtime;

-- 5. A collector group's variable group.
alter table opamp_server.collector_groups
  add column variable_group_id uuid null,
  add column variable_group_stamp bigint null,
  add constraint fk_collector_groups_variable_group
    foreign key (organization_id, variable_group_id)
    references opamp_server.variable_groups (organization_id, id),
  add constraint ck_collector_groups_variable_group_stamp_positive
    check (variable_group_stamp is null or variable_group_stamp >= 1);

comment on column opamp_server.collector_groups.variable_group_id is
  'the variable group whose overrides the group''s collectors are filled in
   with, under their pin''s, or null for none. Always the organization''s
   own, and while set the variable group can''t be deleted.';
comment on column opamp_server.collector_groups.variable_group_stamp is
  'the organization''s counter value when variable_group_id was last set,
   changed or cleared, or null if it never was: the group''s collectors read
   Pending until served past it. Clearing the binding keeps the stamp: the
   id becomes null, the stamp keeps its value.';
comment on constraint fk_collector_groups_variable_group on opamp_server.collector_groups is
  'a group''s variable group is the organization''s own, and can''t be
   deleted while a group uses it (NO ACTION, answered 409
   variable_group_in_use). Null skips it.';
comment on constraint ck_collector_groups_variable_group_stamp_positive on opamp_server.collector_groups is
  'a stamp is a counter value after its bump, so at least 1, or null.';

create index idx_collector_groups_variable_group_id
  on opamp_server.collector_groups (organization_id, variable_group_id)
  where variable_group_id is not null;

comment on index opamp_server.idx_collector_groups_variable_group_id is
  'the groups that use a variable group: the NO ACTION check when one is
   deleted, and its Used by. Its name leaves out the leading
   organization_id.';

grant update (variable_group_id, variable_group_stamp)
  on opamp_server.collector_groups to telflo_opamp_server_runtime;

-- 6. A collector's pinned variable group and its served-variables marker.
alter table opamp_server.collector_instances
  add column pinned_variable_group_id uuid null,
  add column pinned_variable_group_stamp bigint null,
  add column served_variables_stamp bigint null,
  add constraint fk_collector_instances_pinned_variable_group
    foreign key (organization_id, pinned_variable_group_id)
    references opamp_server.variable_groups (organization_id, id),
  add constraint ck_collector_instances_pinned_variable_group_stamp_positive
    check (pinned_variable_group_stamp is null or pinned_variable_group_stamp >= 1),
  add constraint ck_collector_instances_served_variables_stamp_non_negative
    check (served_variables_stamp is null or served_variables_stamp >= 0);

comment on column opamp_server.collector_instances.pinned_variable_group_id is
  'a variable group pinned on it, whose overrides win over its group''s
   variable group''s, or null. Independent of pinned_publication_id: either,
   both or neither. Always the organization''s own, and while pinned the
   variable group can''t be deleted.';
comment on column opamp_server.collector_instances.pinned_variable_group_stamp is
  'the organization''s counter value when pinned_variable_group_id was last
   set, changed or cleared, and also when the collector moved between groups
   whose variable groups differ (a move''s stamp), or null if neither ever
   happened. Clearing the pin keeps the stamp: the id becomes null, the
   stamp keeps its value.';
comment on column opamp_server.collector_instances.served_variables_stamp is
  'the marker: the newest change stamp among the values it was last served
   with, written by the poll that serves, or null before any. It reads
   Pending while a value its text uses has a newer stamp. In no index, so
   its write stays a HOT update (00009''s fillfactor 80).';
comment on constraint fk_collector_instances_pinned_variable_group on opamp_server.collector_instances is
  'a pinned variable group is the organization''s own, and can''t be deleted
   while pinned (NO ACTION, answered 409 variable_group_in_use). Null skips
   it.';
comment on constraint ck_collector_instances_pinned_variable_group_stamp_positive on opamp_server.collector_instances is
  'a stamp is a counter value after its bump, so at least 1, or null.';
comment on constraint ck_collector_instances_served_variables_stamp_non_negative on opamp_server.collector_instances is
  'the marker is 0 or more (0 when its text used no value), or null.';

create index idx_collector_instances_pinned_variable_group_id
  on opamp_server.collector_instances (organization_id, pinned_variable_group_id)
  where pinned_variable_group_id is not null;

comment on index opamp_server.idx_collector_instances_pinned_variable_group_id is
  'the collectors a variable group is pinned on: the NO ACTION check when
   one is deleted, and its Used by. Its name leaves out the leading
   organization_id.';

grant update (pinned_variable_group_id, pinned_variable_group_stamp, served_variables_stamp)
  on opamp_server.collector_instances to telflo_opamp_server_runtime;

-- 7. The names each published version references.
create table opamp_server.publication_variable_names (
  organization_id  uuid  not null,
  publication_id   uuid  not null,
  name             text  not null,
  constraint publication_variable_names_pkey
    primary key (organization_id, publication_id, name),
  constraint fk_publication_variable_names_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_publication_variable_names_publication
    foreign key (organization_id, publication_id)
    references opamp_server.publications (organization_id, id)
    on delete cascade,
  constraint ck_publication_variable_names_name_format
    check (name ~ '^[A-Za-z_][A-Za-z0-9_]*$' and char_length(name) <= 128)
);

comment on table opamp_server.publication_variable_names is
  'the NAMEs of the live ${org:NAME} references in each published version''s
   YAML, written at publish only: a version published before migration 00012
   has none until it is republished. Read by the in-use guards, the
   undefined check and the targeted status. Never updated: it goes with its
   published version.';
comment on column opamp_server.publication_variable_names.organization_id is
  'the organization it belongs to: every query filters on it first, and its
   link carries it.';
comment on column opamp_server.publication_variable_names.publication_id is
  'the published version that references the name, the organization''s
   own. The row goes with it.';
comment on column opamp_server.publication_variable_names.name is
  'a NAME the published version references, once per version: the
   renderer''s grammar, at most 128 characters, as a variable''s name (a
   published version referencing a longer one is refused at publish).
   Configuration text: never logged.';

comment on constraint publication_variable_names_pkey on opamp_server.publication_variable_names is
  'each name once per published version, led by organization_id. Its index
   serves the published version''s cascade.';
comment on constraint fk_publication_variable_names_organization on opamp_server.publication_variable_names is
  'the row goes with its organization, in the same pass as every other
   table''s cascade.';
comment on constraint fk_publication_variable_names_publication on opamp_server.publication_variable_names is
  'a name is of a published version of its own organization, and goes with
   it, by the cascade (the server''s role has no delete).';
comment on constraint ck_publication_variable_names_name_format on opamp_server.publication_variable_names is
  'a name is the renderer''s grammar, ASCII, at most 128 characters, as a
   variable''s: the bound keeps every entry of the primary key''s and the
   name''s btree within Postgres''s limit, and a publish referencing a
   longer name is refused before it writes.';

create index idx_publication_variable_names_name
  on opamp_server.publication_variable_names (organization_id, name);

comment on index opamp_server.idx_publication_variable_names_name is
  'the published versions that reference a name: the in-use guard of a
   variable''s delete or rename. Its name leaves out the leading
   organization_id.';

grant select, insert on opamp_server.publication_variable_names to telflo_opamp_server_runtime;

-- +goose Down
revoke select, insert on opamp_server.publication_variable_names from telflo_opamp_server_runtime;
drop table opamp_server.publication_variable_names;

revoke update (pinned_variable_group_id, pinned_variable_group_stamp, served_variables_stamp)
  on opamp_server.collector_instances from telflo_opamp_server_runtime;
drop index opamp_server.idx_collector_instances_pinned_variable_group_id;
alter table opamp_server.collector_instances
  drop constraint ck_collector_instances_served_variables_stamp_non_negative,
  drop constraint ck_collector_instances_pinned_variable_group_stamp_positive,
  drop constraint fk_collector_instances_pinned_variable_group,
  drop column served_variables_stamp,
  drop column pinned_variable_group_stamp,
  drop column pinned_variable_group_id;

revoke update (variable_group_id, variable_group_stamp)
  on opamp_server.collector_groups from telflo_opamp_server_runtime;
drop index opamp_server.idx_collector_groups_variable_group_id;
alter table opamp_server.collector_groups
  drop constraint ck_collector_groups_variable_group_stamp_positive,
  drop constraint fk_collector_groups_variable_group,
  drop column variable_group_stamp,
  drop column variable_group_id;

revoke update (value, changed_by_id, changed_by_email, changed_at, stamp)
  on opamp_server.variable_group_overrides from telflo_opamp_server_runtime;
revoke select, insert, delete on opamp_server.variable_group_overrides from telflo_opamp_server_runtime;
drop table opamp_server.variable_group_overrides;

revoke update (name, description, changed_by_id, changed_by_email, changed_at)
  on opamp_server.variable_groups from telflo_opamp_server_runtime;
revoke select, insert, delete on opamp_server.variable_groups from telflo_opamp_server_runtime;
drop table opamp_server.variable_groups;

revoke update (name, description, value, changed_by_id, changed_by_email, changed_at, stamp)
  on opamp_server.variables from telflo_opamp_server_runtime;
revoke select, insert, delete on opamp_server.variables from telflo_opamp_server_runtime;
drop table opamp_server.variables;

alter table opamp_server.organizations
  drop column generation;

revoke update (variables_revision) on opamp_server.organizations from telflo_opamp_server_runtime;
alter table opamp_server.organizations
  drop constraint ck_organizations_variables_revision_non_negative,
  drop column variables_revision;
