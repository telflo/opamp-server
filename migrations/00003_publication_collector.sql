-- A publication's collector.
--
-- Each publication now says which collector it was published for: the
-- distribution and the version, as the platform's catalogue names them
-- (`otelcol-contrib`, `0.162.0`). They are plain values, as every platform
-- id here is: the server holds no catalogue, and resolves no image or
-- binaries from the two.
--
--   * Expand only. Both columns are not null with a default, so a server of
--     the previous release, which inserts without them, keeps storing
--     publications while this goes in and until every server runs this
--     release (its rows read Contrib and 0.162.0, the platform's default).
--     This release's store writes both on every insert and never relies on
--     the defaults; 00004, the contract step, drops them.
--   * Existing publications are filled by the defaults: OpenTelemetry
--     Collector Contrib 0.162.0, the version every platform version was
--     moved to. ADD COLUMN with a constant default rewrites no row and
--     fires no trigger.
--   * The checks are openapi.yaml's formats, written with [0-9] rather than
--     \d so the server, the platform's fake (ajv) and Postgres agree: a
--     distribution is lowercase words joined by single hyphens, at most 64
--     characters; a version is major.minor.patch, at most 64 characters.
--   * The table stays immutable: telflo_opamp_runtime's grants are 00001's
--     (select, insert and delete), which cover the new columns. Nothing is
--     granted or revoked here.
--
-- Down drops the two columns (their checks go with them), and with them
-- every publication's collector, and restores the table's comment.

-- +goose Up
alter table telflo_opamp.publications
  add column collector_distribution text not null default 'otelcol-contrib',
  add column collector_version      text not null default '0.162.0',
  add constraint ck_publications_collector_distribution_format
    check (collector_distribution ~ '^[a-z0-9]+(-[a-z0-9]+)*$'
           and char_length(collector_distribution) <= 64),
  add constraint ck_publications_collector_version_format
    check (collector_version ~ '^[0-9]+\.[0-9]+\.[0-9]+$'
           and char_length(collector_version) <= 64);

comment on table telflo_opamp.publications is
  'an immutable, self-describing snapshot of one configuration version, as
   the platform published it, with the collector it was published for. Never
   updated: a new publish is a new row, and a delete is a hard delete. Its
   one foreign key is to its organization, inside the schema; none leaves
   it.';
comment on column telflo_opamp.publications.collector_distribution is
  'the collector distribution the configuration was published for, as the
   platform''s catalogue names it (e.g. otelcol-contrib): a plain value, no
   foreign key. The default is for a server of the previous release only
   (expand); the store always writes it, and a later migration drops the
   default.';
comment on column telflo_opamp.publications.collector_version is
  'the collector version the configuration was published for,
   major.minor.patch (e.g. 0.162.0), a version of collector_distribution in
   the platform''s catalogue: a plain value. The default is for a server of
   the previous release only (expand); the store always writes it, and a
   later migration drops the default.';
comment on constraint ck_publications_collector_distribution_format on telflo_opamp.publications is
  'openapi.yaml''s Collector.distribution: lowercase letters and digits in
   words joined by single hyphens, at most 64 characters.';
comment on constraint ck_publications_collector_version_format on telflo_opamp.publications is
  'openapi.yaml''s Collector.version: major.minor.patch in decimal digits, at
   most 64 characters.';

-- +goose Down
alter table telflo_opamp.publications
  drop column collector_version,
  drop column collector_distribution;
comment on table telflo_opamp.publications is
  'an immutable, self-describing snapshot of one configuration version, as
   the platform published it. Never updated: a new publish is a new row, and
   a delete is a hard delete. Its one foreign key is to its organization,
   inside the schema; none leaves it.';
