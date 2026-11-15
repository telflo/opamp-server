-- Publications.
--
-- A publication is an immutable, self-describing snapshot of one version of
-- a configuration, as the platform published it: its YAML, its digest, and
-- the names the platform had for it at that moment. Collectors are served
-- from these rows, so nothing here may depend on the platform's database,
-- which this server can't reach and doesn't know.
--
--   * organization_id is an opaque tenant key: no foreign key, no names, no
--     members. Every query takes it as its first filter, and the index
--     leads with it.
--   * No foreign key at all, here or later, leaves opamp_server: the
--     configuration and version ids are the platform's, kept as plain
--     values. The platform's database isn't reachable from this one in any
--     case.
--   * Immutable: telflo_opamp_server_runtime gets select, insert and delete, and
--     no update. A new publish is a new row; deleting is a hard delete.
--   * No column has a default: the store writes every one, and received_at
--     comes from the server's injected clock, never now() (CONTRIBUTING.md,
--     "Time").
--   * telflo_opamp_server_runtime may read goose's version table, so `serve` can
--     refuse to start against a schema that is behind or ahead of its build.
--
-- Down drops the table and revokes every grant Up made, leaving the schema
-- and the version table to goose.

-- +goose Up
create table opamp_server.publications (
  id                  uuid         primary key,
  organization_id     uuid         not null,
  configuration_id    uuid         not null,
  configuration_name  text         not null,
  version_id          uuid         not null,
  version_number      integer      not null,
  version_label       text         null,
  yaml                text         not null,
  sha256              text         not null,
  published_by_id     uuid         null,
  published_by_email  text         null,
  published_at        timestamptz  not null,
  received_at         timestamptz  not null,
  constraint ck_publications_configuration_name_length
    check (char_length(configuration_name) between 1 and 200),
  constraint ck_publications_version_number_positive
    check (version_number >= 1),
  constraint ck_publications_version_label_length
    check (version_label is null or char_length(version_label) between 1 and 80),
  constraint ck_publications_sha256_hex
    check (sha256 ~ '^[0-9a-f]{64}$'),
  constraint ck_publications_published_by_both_or_neither
    check ((published_by_id is null) = (published_by_email is null))
);

comment on table opamp_server.publications is
  'an immutable, self-describing snapshot of one configuration version, as
   the platform published it. Never updated: a new publish is a new row, and
   a delete is a hard delete. No foreign key, in or out.';
comment on column opamp_server.publications.id is
  'chosen by the caller, fresh for every publish attempt, so a retried
   publish after a failure leaves at most a harmless orphan.';
comment on column opamp_server.publications.organization_id is
  'an opaque tenant key supplied by the integrator: never interpreted, with
   no foreign key, names or members. Every query filters on it first.';
comment on column opamp_server.publications.configuration_id is
  'the platform''s configuration id, kept as a plain value: no foreign key
   can leave this database.';
comment on column opamp_server.publications.configuration_name is
  'the configuration''s name when it was published (1 to 200 characters), a
   snapshot: renaming the configuration later does not change it.';
comment on column opamp_server.publications.version_id is
  'the platform''s configuration version id, a plain value with no foreign
   key.';
comment on column opamp_server.publications.version_number is
  'the version''s number in its configuration, from 1, as the platform
   numbered it.';
comment on column opamp_server.publications.version_label is
  'the version''s name when it was published, or null for an unnamed one
   (1 to 80 characters when present), a snapshot.';
comment on column opamp_server.publications.yaml is
  'the collector configuration served to collectors, exactly as published.';
comment on column opamp_server.publications.sha256 is
  'the YAML''s SHA-256, lowercase hex, so a collector''s reported hash can be
   compared without hashing the YAML again.';
comment on column opamp_server.publications.published_by_id is
  'the platform''s id for the person who published, a plain value with no
   foreign key. Null with published_by_email, for a publish no person made.';
comment on column opamp_server.publications.published_by_email is
  'the publisher''s email when they published, a snapshot. Null exactly when
   published_by_id is.';
comment on column opamp_server.publications.published_at is
  'when the platform published it, by the platform''s clock.';
comment on column opamp_server.publications.received_at is
  'when this server stored it, by the server''s injected clock, never
   now().';

create index idx_publications_organization_id_configuration_id
  on opamp_server.publications (organization_id, configuration_id, published_at desc, id desc);

comment on index opamp_server.idx_publications_organization_id_configuration_id is
  'a configuration''s publications, newest first, within its organization:
   the listing and the latest-publication lookups. Led by organization_id,
   as every query is. id breaks ties between equal published_at values, so
   the order is total.';

grant usage on schema opamp_server to telflo_opamp_server_runtime;
grant select, insert, delete on opamp_server.publications to telflo_opamp_server_runtime;
grant select on opamp_server.goose_db_version to telflo_opamp_server_runtime;

-- +goose Down
revoke select on opamp_server.goose_db_version from telflo_opamp_server_runtime;
drop table opamp_server.publications;
revoke usage on schema opamp_server from telflo_opamp_server_runtime;
