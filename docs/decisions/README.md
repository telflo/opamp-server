# Decisions

The opamp-server's architecture decision records: each one says what was
decided, why, what was rejected and what follows. A decision changes only
by a new record, or an amendment written into the one it changes.

| Record | Decision |
|---|---|
| [0001](0001-reached-only-through-its-api.md) | The server is reached only through its API: its own database, schema and roles, `CONNECT` revoked from `PUBLIC`, migrations as a separate step, the organization as an opaque tenant key, the management port behind a shared secret. |
| [0002](0002-the-agent-tier.md) | The agent tier: its own port behind its own load balancer, stateless replicas, two token kinds under a key only the server holds, isolation by composite keys. |
| [0003](0003-api-keys.md) | API keys beside the shared secret: minted and revoked by the secret's holder, each limited to its organizations and expiring within 2 years, checked against the database on every request; `api_keys`, the one table of no organization. |
