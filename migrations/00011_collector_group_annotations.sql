-- A collector group's annotations.
--
-- The management client's own data about a group, stored and returned
-- unchanged (in the canonical form the stores answer: keys sorted, no
-- insignificant space), never read by this server: a JSON object of at most
-- 16,384 bytes as jsonb text, or null, on any fleet's group. Like
-- Kubernetes' annotations, each client keeps its data under keys of its own.
--
-- Expand only: a nullable column with no default, so a server of the
-- previous release (whose inserts name their columns and leave it null, and
-- whose reads don't select it) keeps working while this goes in.
--
-- First it refuses a database migrated before 00009's edit in place. That
-- edit deleted the imports migration and renumbered the collectors' to 9,
-- and 00010 reuses that number, so goose finds such a database (at 10: the
-- old 9, imports, and 10, collectors with import columns) with only this
-- migration pending. Its telflo_opamp.imports table, which no current
-- migration makes, marks it. Refusing here, in this migration's
-- transaction, leaves it at 10 (one left at the old 9 is refused by 10's
-- same guard); `serve` refuses either (store/database CheckStartup).
--
-- Down revokes the grant Up made and drops the column.

-- +goose Up
-- +goose StatementBegin
do $$
begin
  if to_regclass('telflo_opamp.imports') is not null then
    raise exception 'this database holds the old imports table (telflo_opamp.imports): recreate it (see the opamp-server README, "A database holding the old imports table")'
      using errcode = 'object_not_in_prerequisite_state';
  end if;
end
$$;
-- +goose StatementEnd

alter table telflo_opamp.collector_groups
  add column annotations jsonb null,
  add constraint ck_collector_groups_annotations_bounded
    check (annotations is null
           or (jsonb_typeof(annotations) = 'object'
               and octet_length(annotations::text) <= 16384));

comment on column telflo_opamp.collector_groups.annotations is
  'the management client''s own data about the group, stored and returned
   unchanged; the server never reads it. A JSON object of at most 16 KiB, or
   null, on any fleet''s group. Configuration text (it may hold paths):
   never logged.';
comment on constraint ck_collector_groups_annotations_bounded on telflo_opamp.collector_groups is
  'annotations is a JSON object of at most 16 KiB (as jsonb writes it back),
   or null.';

grant update (annotations) on telflo_opamp.collector_groups to telflo_opamp_runtime;

-- +goose Down
revoke update (annotations) on telflo_opamp.collector_groups from telflo_opamp_runtime;
alter table telflo_opamp.collector_groups drop column annotations;
