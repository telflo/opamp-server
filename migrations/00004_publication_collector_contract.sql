-- A publication's collector, contracted.
--
-- 00003 gave collector_distribution and collector_version defaults so that
-- a server of the release before it, which inserted without them, could
-- keep storing publications while it went in (expand). Every server since
-- 00003 writes both columns on every insert
-- (store/publications/postgres.go), so the previous release keeps working
-- when they go (contract): the defaults are dropped, and an insert that
-- leaves either column out now fails (23502) rather than recording a
-- collector nobody chose.
--
--   * No row changes: dropping a default rewrites nothing.
--   * Nothing is granted or revoked: the table's grants are 00001's.
--
-- Down puts the defaults back (00003's state) and 00003's comments with
-- them.

-- +goose Up
alter table opamp_server.publications
  alter column collector_distribution drop default,
  alter column collector_version drop default;

comment on column opamp_server.publications.collector_distribution is
  'the collector distribution the configuration was published for, as the
   platform''s catalogue names it (e.g. otelcol-contrib): a plain value, no
   foreign key. No default: the store writes it on every insert (00004, the
   contract step after 00003''s expand).';
comment on column opamp_server.publications.collector_version is
  'the collector version the configuration was published for,
   major.minor.patch (e.g. 0.162.0), a version of collector_distribution in
   the platform''s catalogue: a plain value. No default: the store writes it
   on every insert (00004, the contract step after 00003''s expand).';

-- +goose Down
alter table opamp_server.publications
  alter column collector_distribution set default 'otelcol-contrib',
  alter column collector_version set default '0.162.0';
comment on column opamp_server.publications.collector_distribution is
  'the collector distribution the configuration was published for, as the
   platform''s catalogue names it (e.g. otelcol-contrib): a plain value, no
   foreign key. The default is for a server of the previous release only
   (expand); the store always writes it, and a later migration drops the
   default.';
comment on column opamp_server.publications.collector_version is
  'the collector version the configuration was published for,
   major.minor.patch (e.g. 0.162.0), a version of collector_distribution in
   the platform''s catalogue: a plain value. The default is for a server of
   the previous release only (expand); the store always writes it, and a
   later migration drops the default.';
