-- Organizations.
--
-- The organization is still an opaque id the integrator supplies, but the
-- server now keeps a registry of them: a publication may only be stored
-- under an organization registered first, so a typo or a wrong variable can
-- no longer create an orphan tenant.
--
--   * The name is display-only (1 to 400 characters: a personal
--     organization's name carries its owner's email); the platform owns the
--     real one. Postgres's text can't hold U+0000, and the API refuses it.
--   * publications.organization_id gets a foreign key to organizations,
--     inside opamp_server (no foreign key ever leaves the schema), with on
--     delete cascade: deleting an organization deletes everything it owns.
--     The cascade finds an organization's publications through
--     idx_publications_organization_id_configuration_id, which leads with
--     organization_id.
--   * Expand only. Every organization the existing publications name is
--     registered first, named by its id's text (the server never knew its
--     name; the platform's first publish renames it), so the foreign key
--     holds for every existing row. The table is locked against writes
--     first, so no publication can arrive under a new organization between
--     the backfill and the foreign key. A server of the previous release
--     that keeps serving while this goes in still stores and reads every
--     publication of those organizations; a PUT under an organization
--     nobody registered now fails, which is the point. That server has no
--     answer for the foreign key's refusal, so until every pod runs this
--     release it answers such a PUT with a plain 500 rather than 404
--     organization_not_found.
--   * No column has a default: the store writes every one from the server's
--     injected clock, never now(). The backfill, which has no clock, takes
--     the organization's first publication's received_at: the moment the
--     server first knew of it.
--   * telflo_opamp_server_runtime gets select, insert and delete on the table, and
--     update on name and updated_at only: a rename. The id and created_at
--     never change.
--
-- Down drops the foreign key and the table (the registry's names are lost;
-- the publications stay), restores the publications table's comments, and
-- revokes every grant Up made.

-- +goose Up
lock table opamp_server.publications in share row exclusive mode;

create table opamp_server.organizations (
  id          uuid         primary key,
  name        text         not null,
  created_at  timestamptz  not null,
  updated_at  timestamptz  not null,
  constraint ck_organizations_name_length
    check (char_length(name) between 1 and 400)
);

comment on table opamp_server.organizations is
  'the organizations this server stores anything for: a registry of the
   integrator''s opaque tenant ids. Registered before any publication is
   stored under one; deleting one deletes everything it owns.';
comment on column opamp_server.organizations.id is
  'the integrator''s organization id (the platform passes its own), or one
   the server generated when the caller gave none. Never interpreted.';
comment on column opamp_server.organizations.name is
  'a display name, 1 to 400 characters, as the caller last sent it; the
   platform owns the real one. Backfilled organizations are named by their
   id until renamed.';
comment on column opamp_server.organizations.created_at is
  'when this server registered it, by the server''s injected clock, never
   now(). For a backfilled organization, its first publication''s
   received_at.';
comment on column opamp_server.organizations.updated_at is
  'when its name last changed, by the server''s injected clock; created_at
   until it is renamed.';

-- Every organization the existing publications name, before the foreign key.
insert into opamp_server.organizations (id, name, created_at, updated_at)
select organization_id, organization_id::text, min(received_at), min(received_at)
  from opamp_server.publications
 group by organization_id;

alter table opamp_server.publications
  add constraint fk_publications_organization
  foreign key (organization_id) references opamp_server.organizations (id)
  on delete cascade;

comment on constraint fk_publications_organization on opamp_server.publications is
  'a publication belongs to a registered organization, and goes with it.
   Inside the schema: no foreign key leaves it.';
comment on table opamp_server.publications is
  'an immutable, self-describing snapshot of one configuration version, as
   the platform published it. Never updated: a new publish is a new row, and
   a delete is a hard delete. Its one foreign key is to its organization,
   inside the schema; none leaves it.';
comment on column opamp_server.publications.organization_id is
  'the organization it belongs to, registered in organizations first: an
   opaque tenant key the integrator supplies, never interpreted. Every query
   filters on it first.';

grant select, insert, delete on opamp_server.organizations to telflo_opamp_server_runtime;
grant update (name, updated_at) on opamp_server.organizations to telflo_opamp_server_runtime;

-- +goose Down
revoke update (name, updated_at) on opamp_server.organizations from telflo_opamp_server_runtime;
revoke select, insert, delete on opamp_server.organizations from telflo_opamp_server_runtime;
alter table opamp_server.publications drop constraint fk_publications_organization;
comment on table opamp_server.publications is
  'an immutable, self-describing snapshot of one configuration version, as
   the platform published it. Never updated: a new publish is a new row, and
   a delete is a hard delete. No foreign key, in or out.';
comment on column opamp_server.publications.organization_id is
  'an opaque tenant key supplied by the integrator: never interpreted, with
   no foreign key, names or members. Every query filters on it first.';
drop table opamp_server.organizations;
