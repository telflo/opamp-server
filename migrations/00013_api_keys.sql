-- API keys: management API credentials other than the shared secret.
--
-- The secret holder (the platform, or an operator) mints them; each opens
-- the management API's routes for the organizations it names, or for
-- every organization, until it expires or is revoked. The key itself is
-- never stored: only the SHA-256 of its secret part, which is 32 random
-- bytes, so a plain hash is enough.
--
--   * api_keys is the server's own table, the first: a key belongs to no
--     organization, so it has no organization_id and no cascade from one,
--     and the link to it carries none. Everything else in the schema is an
--     organization's.
--   * all_organizations true opens every organization, registered now or
--     later; false opens only the ones api_key_organizations names. An
--     organization's delete takes its links (the cascade), never the key,
--     so a key whose organizations are all gone opens nothing.
--   * A key is never deleted: a revoke sets revoked_at, and the row is
--     kept, with its name, as the record of who held what. telflo_opamp_server_runtime
--     may update its name, description, revoked_at and last_used_at only, so
--     its secret, its organizations and its expiry never change.
--   * No column has a default; times from the server's injected clock.
--
-- Down drops both tables and revokes every grant Up made.

-- +goose Up
create table opamp_server.api_keys (
  id                 uuid         primary key,
  name               text         not null,
  description        text         null,
  secret_sha256      bytea        not null,
  all_organizations  boolean      not null,
  expires_at         timestamptz  not null,
  revoked_at         timestamptz  null,
  last_used_at       timestamptz  null,
  created_at         timestamptz  not null,
  constraint uq_api_keys_secret_sha256 unique (secret_sha256),
  constraint ck_api_keys_name_length
    check (char_length(name) between 1 and 80),
  constraint ck_api_keys_description_length
    check (description is null or char_length(description) between 1 and 1000),
  constraint ck_api_keys_secret_sha256_length
    check (octet_length(secret_sha256) = 32),
  constraint ck_api_keys_expires_after_created
    check (expires_at > created_at)
);

comment on table opamp_server.api_keys is
  'the management API''s keys: credentials the secret holder mints, each
   opening the routes of the organizations it names, or of every one, until
   it expires or is revoked. The server''s own table, of no organization.
   The key is never stored, only its secret''s SHA-256. Never deleted.';
comment on column opamp_server.api_keys.id is
  'the key''s id, written in the key itself: the lookup, then the hash is
   compared.';
comment on column opamp_server.api_keys.name is
  'what the secret holder calls it, 1 to 80 characters. Not unique.';
comment on column opamp_server.api_keys.description is
  'what it is for, 1 to 1,000 characters, or null.';
comment on column opamp_server.api_keys.secret_sha256 is
  'the SHA-256 of the key''s secret part (32 random bytes). Never changes:
   the runtime role has no update on it.';
comment on column opamp_server.api_keys.all_organizations is
  'true: it opens every organization, registered now or later; false: only
   those api_key_organizations names. Never changes.';
comment on column opamp_server.api_keys.expires_at is
  'when it stops authenticating; every key expires. Never changes.';
comment on column opamp_server.api_keys.revoked_at is
  'when it was revoked, by the injected clock, or null. A revoked key never
   authenticates; the first revoke''s time is kept.';
comment on column opamp_server.api_keys.last_used_at is
  'when it last authenticated a request, by the injected clock, or null for
   never.';
comment on column opamp_server.api_keys.created_at is
  'when the server made it, by the injected clock, never now().';

comment on constraint api_keys_pkey on opamp_server.api_keys is
  'a key''s id is unique on its own: the lookup, by the id in the key.';
comment on constraint uq_api_keys_secret_sha256 on opamp_server.api_keys is
  'no two keys share a secret.';
comment on constraint ck_api_keys_name_length on opamp_server.api_keys is
  'a name is 1 to 80 characters.';
comment on constraint ck_api_keys_description_length on opamp_server.api_keys is
  'a description is 1 to 1,000 characters, or null.';
comment on constraint ck_api_keys_secret_sha256_length on opamp_server.api_keys is
  'a SHA-256 is 32 bytes.';
comment on constraint ck_api_keys_expires_after_created on opamp_server.api_keys is
  'a key expires after it is made.';

create index idx_api_keys_created_at
  on opamp_server.api_keys (created_at desc, id desc);

comment on index opamp_server.idx_api_keys_created_at is
  'the keys newest first: the list. id breaks ties.';

grant select, insert on opamp_server.api_keys to telflo_opamp_server_runtime;
grant update (name, description, revoked_at, last_used_at) on opamp_server.api_keys to telflo_opamp_server_runtime;

create table opamp_server.api_key_organizations (
  organization_id  uuid  not null,
  api_key_id       uuid  not null,
  constraint api_key_organizations_pkey primary key (api_key_id, organization_id),
  constraint fk_api_key_organizations_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_api_key_organizations_api_key
    foreign key (api_key_id) references opamp_server.api_keys (id)
    on delete cascade
);

comment on table opamp_server.api_key_organizations is
  'the organizations a key limited to some opens, one row each. Written
   with the key, never changed; an organization''s delete takes its rows.';
comment on column opamp_server.api_key_organizations.organization_id is
  'an organization the key opens; the row goes with it.';
comment on column opamp_server.api_key_organizations.api_key_id is
  'the key.';

comment on constraint api_key_organizations_pkey on opamp_server.api_key_organizations is
  'a key names an organization once; read by the key, at every request
   it authenticates.';
comment on constraint fk_api_key_organizations_organization on opamp_server.api_key_organizations is
  'the link goes with its organization, in the same pass as every other
   table''s cascade: the key then no longer opens it.';
comment on constraint fk_api_key_organizations_api_key on opamp_server.api_key_organizations is
  'the link names a key, the server''s own row, so it carries no
   organization_id; keys are never deleted but by the down.';

create index idx_api_key_organizations_organization_id
  on opamp_server.api_key_organizations (organization_id);

comment on index opamp_server.idx_api_key_organizations_organization_id is
  'how an organization''s cascade finds its links.';

grant select, insert on opamp_server.api_key_organizations to telflo_opamp_server_runtime;

-- +goose Down
revoke select, insert on opamp_server.api_key_organizations from telflo_opamp_server_runtime;
drop table opamp_server.api_key_organizations;
revoke update (name, description, revoked_at, last_used_at) on opamp_server.api_keys from telflo_opamp_server_runtime;
revoke select, insert on opamp_server.api_keys from telflo_opamp_server_runtime;
drop table opamp_server.api_keys;
