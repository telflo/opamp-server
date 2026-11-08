-- Fleets.
--
-- A fleet is a set of collectors that enroll with the same tokens and are
-- managed together. Server-class types only: `server` and
-- `kubernetes-supervisor`.
--
--   * unique (organization_id, id) is the target of every composite foreign
--     key naming a fleet: a group, collector, token or token event can only
--     name its own organization's fleet.
--   * Its organization by a direct (organization_id) -> organizations(id)
--     on delete cascade, as every table of the fleet model has: deleting an
--     organization deletes its fleets and, through them, everything they
--     hold.
--   * The label is trimmed (no leading or trailing whitespace of any kind:
--     a space, a tab, a line break) and 1 to 80 characters, unique per
--     organization as written (case-sensitive).
--   * type never changes: telflo_opamp_runtime has no update on it (a
--     42501). label, status and updated_at may change.
--   * No column has a default: the store writes every one, the times from
--     the server's injected clock, never now().
--
-- Down drops the table and revokes every grant Up made.

-- +goose Up
create table telflo_opamp.fleets (
  id                uuid         primary key,
  organization_id   uuid         not null,
  label             text         not null,
  type              text         not null,
  status            text         not null,
  created_by_id     uuid         null,
  created_by_email  text         null,
  created_at        timestamptz  not null,
  updated_at        timestamptz  not null,
  constraint uq_fleets_organization_id_id unique (organization_id, id),
  constraint uq_fleets_organization_id_label unique (organization_id, label),
  constraint fk_fleets_organization
    foreign key (organization_id) references telflo_opamp.organizations (id)
    on delete cascade,
  constraint ck_fleets_label_trimmed_length
    check (label !~ '^[[:space:]]|[[:space:]]$' and char_length(label) between 1 and 80),
  constraint ck_fleets_type
    check (type in ('server', 'kubernetes-supervisor')),
  constraint ck_fleets_status
    check (status in ('active', 'archived')),
  constraint ck_fleets_created_by_both_or_neither
    check ((created_by_id is null) = (created_by_email is null)),
  constraint ck_fleets_created_by_email_length
    check (created_by_email is null or char_length(created_by_email) between 1 and 320)
);

comment on table telflo_opamp.fleets is
  'a fleet: collectors that enroll with the same tokens and are managed
   together. Server-class types only. Deleting one deletes its groups,
   collectors, tokens and token events.';
comment on column telflo_opamp.fleets.id is
  'the fleet''s id; the caller may choose it, which makes a create
   idempotent. A bearer''s deployment_id names it.';
comment on column telflo_opamp.fleets.organization_id is
  'the organization it belongs to: every query filters on it first, and
   every link to the fleet carries it.';
comment on column telflo_opamp.fleets.label is
  'a display label, trimmed, 1 to 80 characters, unique per organization as
   written (case-sensitive). Configuration text: never logged.';
comment on column telflo_opamp.fleets.type is
  'server or kubernetes-supervisor. Never changes: the runtime role has no
   update on it.';
comment on column telflo_opamp.fleets.status is
  'active or archived; an archived fleet''s collectors are refused.';
comment on column telflo_opamp.fleets.created_by_id is
  'the platform''s id for the person who created it, a plain value with no
   foreign key. Null with created_by_email, for a fleet no person made.';
comment on column telflo_opamp.fleets.created_by_email is
  'the creator''s email when they created it, a snapshot, 1 to 320
   characters. Null exactly when created_by_id is.';
comment on column telflo_opamp.fleets.created_at is
  'when the server created it, by the injected clock, never now().';
comment on column telflo_opamp.fleets.updated_at is
  'when its label or status last changed, by the injected clock; created_at
   until then.';

comment on constraint uq_fleets_organization_id_id on telflo_opamp.fleets is
  'the target of the composite foreign keys of groups, collectors, tokens
   and token events, so each names its own organization''s fleet. Its index
   also serves the organization''s cascade.';
comment on constraint uq_fleets_organization_id_label on telflo_opamp.fleets is
  'a label is unique within its organization, compared as written (409
   fleet_label_taken).';
comment on constraint fk_fleets_organization on telflo_opamp.fleets is
  'a fleet belongs to a registered organization and goes with it. Inside the
   schema: no foreign key leaves it.';
comment on constraint ck_fleets_label_trimmed_length on telflo_opamp.fleets is
  'a label is stored trimmed, with no leading or trailing whitespace (a
   space, a tab, a line break; btrim would strip only spaces), 1 to 80
   characters.';
comment on constraint ck_fleets_type on telflo_opamp.fleets is
  'server-class fleets only: server or kubernetes-supervisor.';
comment on constraint ck_fleets_status on telflo_opamp.fleets is
  'active or archived.';
comment on constraint ck_fleets_created_by_both_or_neither on telflo_opamp.fleets is
  'the creator''s id and email are both known or both null.';
comment on constraint ck_fleets_created_by_email_length on telflo_opamp.fleets is
  'an email is 1 to 320 characters.';
comment on constraint fleets_pkey on telflo_opamp.fleets is
  'a fleet''s id is unique on its own: a bearer names a fleet by id alone.';

create index idx_fleets_created_at
  on telflo_opamp.fleets (organization_id, created_at desc, id desc);

comment on index telflo_opamp.idx_fleets_created_at is
  'an organization''s fleets newest first, keyset-paged. Led by
   organization_id, as every index is; its name leaves it out. id breaks
   ties, so the order is total.';

grant select, insert, delete on telflo_opamp.fleets to telflo_opamp_runtime;
grant update (label, status, updated_at) on telflo_opamp.fleets to telflo_opamp_runtime;

-- +goose Down
revoke update (label, status, updated_at) on telflo_opamp.fleets from telflo_opamp_runtime;
revoke select, insert, delete on telflo_opamp.fleets from telflo_opamp_runtime;
drop table telflo_opamp.fleets;
