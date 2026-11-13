-- Enrollment tokens and their audit events.
--
-- A collector's credential to this server. The bearer is base64(nonce ||
-- AES-256-GCM(JSON) || tag) of {"token_id": <id>, "deployment_id": <fleet
-- id>}, under OPAMP_ENCRYPTION_KEY. The bearer is never stored: a reveal
-- encrypts afresh.
--
--   * id is the bearer's token_id, unique on its own. The agent looks a
--     token up by id alone (a bearer names no organization), then compares
--     the row's fleet with the bearer's.
--   * kind is default (enrollment; never expires) or rotate (served
--     configuration; may expire). One default token per fleet (a partial
--     unique index). A rotation sets the old rotate token's expires_at to
--     the grace end and do_refresh; a revoke sets revoked_at. kind and
--     fleet_id never change: telflo_opamp_server_runtime may update expires_at,
--     revoked_at and do_refresh only, and deletes none (a token goes with
--     its fleet, by the cascade, which runs as the table's owner).
--   * Each table's fleet by (organization_id, fleet_id) -> fleets, on
--     delete cascade, and its organization by a direct (organization_id) ->
--     organizations(id) on delete cascade. unique (organization_id,
--     fleet_id, id) on tokens is the target of a collector's token key and
--     an event's, so a collector's token and an event's are its fleet's.
--   * Events: issued, viewed, rotated or revoked, with ids and the actor
--     only (never a bearer). Each names its token by (organization_id,
--     fleet_id, enrollment_token_id) -> tokens, on delete cascade: an event
--     can only name a token of its own fleet, and since a token goes only
--     with its fleet, which takes its events anyway, no event is lost to
--     it. Immutable: select and insert only.
--   * No column has a default; times from the server's injected clock.
--
-- Down drops both tables and revokes every grant Up made.

-- +goose Up
create table opamp_server.enrollment_tokens (
  id                uuid         primary key,
  organization_id   uuid         not null,
  fleet_id          uuid         not null,
  kind              text         not null,
  expires_at        timestamptz  null,
  revoked_at        timestamptz  null,
  do_refresh        boolean      not null,
  created_by_id     uuid         null,
  created_by_email  text         null,
  created_at        timestamptz  not null,
  constraint uq_enrollment_tokens_organization_id_fleet_id_id unique (organization_id, fleet_id, id),
  constraint fk_enrollment_tokens_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_enrollment_tokens_fleet
    foreign key (organization_id, fleet_id)
    references opamp_server.fleets (organization_id, id)
    on delete cascade,
  constraint ck_enrollment_tokens_kind
    check (kind in ('default', 'rotate')),
  constraint ck_enrollment_tokens_default_never_expires
    check (kind = 'rotate' or expires_at is null),
  constraint ck_enrollment_tokens_created_by_both_or_neither
    check ((created_by_id is null) = (created_by_email is null)),
  constraint ck_enrollment_tokens_created_by_email_length
    check (created_by_email is null or char_length(created_by_email) between 1 and 320)
);

comment on table opamp_server.enrollment_tokens is
  'a fleet''s enrollment tokens, default and rotate: a collector''s
   credential to this server. The bearer is never stored; a reveal encrypts
   afresh. Never deleted but with its fleet.';
comment on column opamp_server.enrollment_tokens.id is
  'the bearer''s token_id, unique on its own: the agent''s lookup key (a
   bearer names no organization).';
comment on column opamp_server.enrollment_tokens.organization_id is
  'the organization it belongs to: every other query filters on it first,
   and every link carries it.';
comment on column opamp_server.enrollment_tokens.fleet_id is
  'the fleet it enrolls into, compared with the bearer''s deployment_id.
   Never changes: the runtime role has no update on it.';
comment on column opamp_server.enrollment_tokens.kind is
  'default (never expires; enrollment only, served no configuration) or
   rotate (served the resolved configuration). Never changes: the runtime
   role has no update on it.';
comment on column opamp_server.enrollment_tokens.expires_at is
  'when it stops authenticating, or null for never. A rotation sets the
   previous rotate token''s to the grace end; a default token never
   expires.';
comment on column opamp_server.enrollment_tokens.revoked_at is
  'when it was revoked, by the injected clock, or null. A revoked token
   never authenticates.';
comment on column opamp_server.enrollment_tokens.do_refresh is
  'whether a collector authenticating with it is pushed the fleet''s newest
   valid rotate token: set on the previous rotate token by a rotation, never
   cleared.';
comment on column opamp_server.enrollment_tokens.created_by_id is
  'the platform''s id for the person who made it, a plain value with no
   foreign key. Null with created_by_email, for a token no person made.';
comment on column opamp_server.enrollment_tokens.created_by_email is
  'its maker''s email then, a snapshot, 1 to 320 characters. Null exactly
   when created_by_id is.';
comment on column opamp_server.enrollment_tokens.created_at is
  'when the server made it, by the injected clock, never now(). The newest
   valid rotate token is the one pushed.';

comment on constraint uq_enrollment_tokens_organization_id_fleet_id_id on opamp_server.enrollment_tokens is
  'the target of a collector''s token key, which carries its fleet, so a
   collector''s token is its own fleet''s. Its index also serves the
   organization''s cascade.';
comment on constraint fk_enrollment_tokens_organization on opamp_server.enrollment_tokens is
  'the token goes with its organization, in the same pass as every other
   table''s cascade.';
comment on constraint fk_enrollment_tokens_fleet on opamp_server.enrollment_tokens is
  'a token belongs to a fleet of its own organization, and goes with it.';
comment on constraint ck_enrollment_tokens_kind on opamp_server.enrollment_tokens is
  'default or rotate.';
comment on constraint ck_enrollment_tokens_default_never_expires on opamp_server.enrollment_tokens is
  'a default token never expires; only a rotate token has an expires_at.';
comment on constraint ck_enrollment_tokens_created_by_both_or_neither on opamp_server.enrollment_tokens is
  'the maker''s id and email are both known or both null.';
comment on constraint ck_enrollment_tokens_created_by_email_length on opamp_server.enrollment_tokens is
  'an email is 1 to 320 characters.';
comment on constraint enrollment_tokens_pkey on opamp_server.enrollment_tokens is
  'a token''s id is unique on its own: the agent''s lookup, by the bearer''s
   token_id alone.';

create unique index uq_enrollment_tokens_fleet_id_where_kind_default
  on opamp_server.enrollment_tokens (organization_id, fleet_id) where kind = 'default';

comment on index opamp_server.uq_enrollment_tokens_fleet_id_where_kind_default is
  'at most one default token per fleet. Its name leaves out the leading
   organization_id.';

create index idx_enrollment_tokens_fleet_id_created_at
  on opamp_server.enrollment_tokens (organization_id, fleet_id, created_at desc, id desc);

comment on index opamp_server.idx_enrollment_tokens_fleet_id_created_at is
  'a fleet''s tokens newest first: the token list, and the newest valid
   rotate token a rotation and a push look for. id breaks ties.';

grant select, insert on opamp_server.enrollment_tokens to telflo_opamp_server_runtime;
grant update (expires_at, revoked_at, do_refresh) on opamp_server.enrollment_tokens to telflo_opamp_server_runtime;

create table opamp_server.enrollment_token_events (
  id                   uuid         primary key,
  organization_id      uuid         not null,
  fleet_id             uuid         not null,
  enrollment_token_id  uuid         not null,
  event                text         not null,
  actor_id             uuid         null,
  actor_email          text         null,
  occurred_at          timestamptz  not null,
  constraint fk_enrollment_token_events_organization
    foreign key (organization_id) references opamp_server.organizations (id)
    on delete cascade,
  constraint fk_enrollment_token_events_fleet
    foreign key (organization_id, fleet_id)
    references opamp_server.fleets (organization_id, id)
    on delete cascade,
  constraint fk_enrollment_token_events_enrollment_token
    foreign key (organization_id, fleet_id, enrollment_token_id)
    references opamp_server.enrollment_tokens (organization_id, fleet_id, id)
    on delete cascade,
  constraint ck_enrollment_token_events_event
    check (event in ('issued', 'viewed', 'rotated', 'revoked')),
  constraint ck_enrollment_token_events_actor_both_or_neither
    check ((actor_id is null) = (actor_email is null)),
  constraint ck_enrollment_token_events_actor_email_length
    check (actor_email is null or char_length(actor_email) between 1 and 320)
);

comment on table opamp_server.enrollment_token_events is
  'the tokens'' audit trail: what happened to which token, when, and who
   did it. Ids and the actor only, never a bearer, label or name.
   Immutable; deleted only with its fleet.';
comment on column opamp_server.enrollment_token_events.id is
  'the event''s id.';
comment on column opamp_server.enrollment_token_events.organization_id is
  'the organization it belongs to: every query filters on it first.';
comment on column opamp_server.enrollment_token_events.fleet_id is
  'the token''s fleet; the event goes with it.';
comment on column opamp_server.enrollment_token_events.enrollment_token_id is
  'the token, always one of its own fleet''s (a composite key). A token
   goes only with its fleet, so the trail is kept for as long as its fleet
   is.';
comment on column opamp_server.enrollment_token_events.event is
  'issued (made, by a create or a rotation), viewed (revealed), rotated (the
   previous rotate token, given a grace end) or revoked.';
comment on column opamp_server.enrollment_token_events.actor_id is
  'the platform''s id for the person who did it, a plain value with no
   foreign key. Null with actor_email, for an event no person caused.';
comment on column opamp_server.enrollment_token_events.actor_email is
  'the actor''s email then, a snapshot, 1 to 320 characters. Null exactly
   when actor_id is.';
comment on column opamp_server.enrollment_token_events.occurred_at is
  'when it happened, by the injected clock, never now().';

comment on constraint fk_enrollment_token_events_organization on opamp_server.enrollment_token_events is
  'the event goes with its organization, in the same pass as every other
   table''s cascade.';
comment on constraint fk_enrollment_token_events_fleet on opamp_server.enrollment_token_events is
  'an event belongs to a fleet of its own organization, and goes with it.';
comment on constraint fk_enrollment_token_events_enrollment_token on opamp_server.enrollment_token_events is
  'an event names a token of its own organization''s fleet, and goes with
   it: a token goes only with its fleet, whose cascade takes the events
   too.';
comment on constraint ck_enrollment_token_events_event on opamp_server.enrollment_token_events is
  'issued, viewed, rotated or revoked.';
comment on constraint ck_enrollment_token_events_actor_both_or_neither on opamp_server.enrollment_token_events is
  'the actor''s id and email are both known or both null.';
comment on constraint ck_enrollment_token_events_actor_email_length on opamp_server.enrollment_token_events is
  'an email is 1 to 320 characters.';
comment on constraint enrollment_token_events_pkey on opamp_server.enrollment_token_events is
  'an event''s id is unique on its own.';

create index idx_enrollment_token_events_fleet_id_occurred_at
  on opamp_server.enrollment_token_events (organization_id, fleet_id, occurred_at desc, id desc);

comment on index opamp_server.idx_enrollment_token_events_fleet_id_occurred_at is
  'a fleet''s events newest first, and how the fleet''s cascade and the
   organization''s find them. id breaks ties.';

grant select, insert on opamp_server.enrollment_token_events to telflo_opamp_server_runtime;

-- +goose Down
revoke select, insert on opamp_server.enrollment_token_events from telflo_opamp_server_runtime;
drop table opamp_server.enrollment_token_events;
revoke update (expires_at, revoked_at, do_refresh) on opamp_server.enrollment_tokens from telflo_opamp_server_runtime;
revoke select, insert on opamp_server.enrollment_tokens from telflo_opamp_server_runtime;
drop table opamp_server.enrollment_tokens;
