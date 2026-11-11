# Run it standalone: Binary

This walks through running opamp-server on one machine from its binary,
starting each part by hand, so you can see what each needs and why. Each
step says what to swap in for your own setup: your PostgreSQL, your
secret store, your URL, your process supervisor. (The
[Quick start](../README.md#quick-start-docker) runs the same parts in Compose with one
command.)

**What you'll run:**

```text
collectors ──▶ opamp-server serve agent       (port 51234) ──┐
                                                             ├──▶ PostgreSQL
you, tools ──▶ opamp-server serve management  (port 51235) ──┘    database telflo_opamp
```

Two processes from one binary: the agent endpoint, which collectors poll,
and the management API, which you call. Both keep everything in one
PostgreSQL database. `opamp-server migrate` creates and upgrades its
tables.

**You need:** `curl`, `openssl`, and Docker for the database (or a
PostgreSQL of your own, step 2).

## 1. Make a folder and get opamp-server

```sh
# Make a folder for the binary and its secrets, open to you alone, and go into it.
mkdir -p ~/opamp-standalone && chmod 700 ~/opamp-standalone && cd ~/opamp-standalone

# Work out this machine's OS and CPU, as the release's files name them.
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')

# Download the latest release for them, and the release's checksums.
curl -fsSLO "https://github.com/telflo/telflo-opamp-server/releases/latest/download/opamp-server_${os}_${arch}.tar.gz"
curl -fsSLO https://github.com/telflo/telflo-opamp-server/releases/latest/download/checksums.txt

# Check the download against its checksum: it answers OK.
grep " opamp-server_${os}_${arch}.tar.gz\$" checksums.txt | shasum -a 256 -c -

# Unpack it: the binary, opamp-server, and the database script, deploy/bootstrap.sql.
tar xzf "opamp-server_${os}_${arch}.tar.gz"
```

**Your own build:** from a checkout, with Go 1.27.3, build the binary into
the folder and copy the script beside it:

```sh
GOTOOLCHAIN=local CGO_ENABLED=0 go build -trimpath -o ~/opamp-standalone/opamp-server .
mkdir -p ~/opamp-standalone/deploy && cp deploy/bootstrap.sql ~/opamp-standalone/deploy/
```

**The image** (`docker build -t opamp-server:dev .`) runs the same
commands as its arguments (`serve agent`, `serve management`,
`migrate up`). It's distroless and runs as a non-root user.

## 2. Set up PostgreSQL

opamp-server keeps everything in one PostgreSQL database, `telflo_opamp`,
and divides the rights over it among three roles: PostgreSQL's
**superuser**, which creates the database and the other two roles; an
**owner**, which creates and upgrades the tables; and a **read/write**
role, which the running server connects as.

Follow [PostgreSQL in Docker for opamp-server](postgres-in-docker.md)
to run it in a container. Each step there explains what it sets up, so
it's also worth reading if you bring your own PostgreSQL. When you're
done, the database and its roles exist, and this folder holds two files:
`migration-database-url` and `database-url`.

**Your own PostgreSQL:** any PostgreSQL 16 or later works, on a server you
already run. That doc's
[last section](postgres-in-docker.md#using-your-own-postgresql-instead)
says what to run on it in place of the container.

## 3. Make the two secrets

```sh
# The management secret: you send it to the management API to create
# organizations and API keys.
openssl rand -hex 32 | tr -d '\n' > management-secret

# The encryption key: it encrypts the enrollment tokens' secrets in the
# database. Back it up: without it, they can't be read again.
openssl rand -hex 32 | tr -d '\n' > encryption-key
```

Each needs at least 32 bytes. `tr -d '\n'` drops `openssl`'s line ending.

**For your own:** take both from your secret store, and give each to the
server as a file only its user can read.
[security.md](security.md) says how to rotate them.

## 4. Create the database's tables

```sh
# As the database's owner, create the tables, or bring them up to this release.
OPAMP_MIGRATION_DATABASE_URL=$(cat migration-database-url) ./opamp-server migrate up
```

Run it again on every upgrade, after a backup. It's safe to run more than
once, and takes a lock, so several at once don't collide.

## 5. Start the agent endpoint

In a terminal of its own:

```sh
cd ~/opamp-standalone

# The database, as the server's own role.
export OPAMP_DATABASE_URL=$(cat database-url)

# The key that decrypts the enrollment tokens' secrets.
export OPAMP_ENCRYPTION_KEY_FILE=$PWD/encryption-key

# The address collectors poll, as they reach it: here, collectors on this machine.
export OPAMP_SERVER_URL=http://localhost:51234/opamp/api/v1

# Start the agent endpoint on port 51234. It never holds the management secret.
./opamp-server serve agent
```

**For your own:** set `OPAMP_SERVER_URL` to the address your collectors reach,
such as `https://opamp.example.com/opamp/api/v1` behind a load balancer
that handles TLS. Or give the endpoint its own certificate with
`OPAMP_AGENT_TLS_CERT_FILE` and `OPAMP_AGENT_TLS_KEY_FILE`.

## 6. Start the management API

In a second terminal:

```sh
cd ~/opamp-standalone

# The same database and key as the agent endpoint.
export OPAMP_DATABASE_URL=$(cat database-url)
export OPAMP_ENCRYPTION_KEY_FILE=$PWD/encryption-key

# The secret the API accepts from you.
export OPAMP_MANAGEMENT_SECRET=$(cat management-secret)

# Listen on this machine only: the API is for you and your tools.
export OPAMP_MANAGEMENT_HOST=127.0.0.1

# Start the management API on port 51235.
./opamp-server serve management
```

**For your own:** the management API is for your platform and your tools, so
keep it off the public network. For HTTPS, set
`OPAMP_MANAGEMENT_TLS_CERT_FILE` and `OPAMP_MANAGEMENT_TLS_KEY_FILE`.

Each `serve` checks its settings, its database role and the schema version
before it opens its port. If anything is wrong, it logs one line saying
what and exits with code 1.

## 7. Check that both are running

In a third terminal:

```sh
# Ask the management API whether it's healthy: it answers ok.
curl http://127.0.0.1:51235/health

# Ask the agent endpoint whether it's ready: it answers ready.
curl http://127.0.0.1:51234/ready
```

## 8. Use it

The Quick start's steps 4 to 12 work against this server, with two
changes:

- **In step 4,** save your management secret in place of the development
  one:
  `printf 'Authorization: Bearer %s\n' "$(cat ~/opamp-standalone/management-secret)" > auth-header`.
- **In step 8,** export `OPAMP_TOKEN` and `COLLECTOR_GROUP_ID` as it
  shows, then start a collector in one of the two ways below in place of
  its `docker compose` command.

**A collector of your own**, however you run it: binaries, a package or a
container. opamp-server talks to the
[OpAMP supervisor](https://github.com/open-telemetry/opentelemetry-collector-contrib/tree/main/cmd/opampsupervisor)
that runs the collector, and the supervisor's configuration needs three
things:

- **The opamp-server URL:** `http://localhost:51234/opamp/api/v1`, the agent
  endpoint, as step 5's `OPAMP_SERVER_URL` names it.
- **The registration token:** an `Authorization: Bearer <token>` header, with the
  fleet's enrollment token (`OPAMP_TOKEN`).
- **The collector group Id:** the non-identifying attribute
  `telflo.collector_group_id`, set to the group's ID
  (`COLLECTOR_GROUP_ID`). The server refuses a collector without it.

[deploy/quickstart-collector/supervisor.yaml](../deploy/quickstart-collector/supervisor.yaml)
is a complete configuration that reads all three from the environment.
For binaries, change its `agent.executable` to your `otelcol-contrib`
and its `storage.directory` to a folder the supervisor can write, export
`OPAMP_ENDPOINT=http://localhost:51234/opamp/api/v1`, and run
`opampsupervisor --config supervisor.yaml`.

If you'd rather test a collector with docker on this machien you can run 
a container locally where `localhost:51234` is your `serve agent`:

```sh
# Build the collector's image from the repository's deploy/quickstart-collector.
docker build -t opamp-quickstart-collector:dev \
  "https://github.com/telflo/telflo-opamp-server.git#main:deploy/quickstart-collector"

# Start it on this machine's network, joining with the token and group you exported.
docker run -d --name opamp-collector --network host \
  -e OPAMP_ENDPOINT=http://localhost:51234/opamp/api/v1 \
  -e OPAMP_TOKEN -e COLLECTOR_GROUP_ID \
  opamp-quickstart-collector:dev

# Follow its log as it connects; Ctrl-C stops following, not the collector.
docker logs -f opamp-collector
```

If you have a clone of the repository, you can build from its folder instead using:
`docker build -t opamp-quickstart-collector:dev deploy/quickstart-collector`.
In the `docker run command`, `--network host` works on Linux. With Docker Desktop on a Mac,
use `host.docker.internal` in place of `localhost` in `OPAMP_ENDPOINT`
here and in step 5's `OPAMP_SERVER_URL`, and leave out `--network host`.

## 9. Run it for real

- **A process supervisor:** run `serve agent` and `serve management` as
  two services (two systemd units, for example), each restarted on its
  own, with its settings from files only the service's user can read.
  Or run both in one process: `opamp-server serve` with all four
  settings. [configuration.md](configuration.md) lists what
  each mode reads, and every optional setting.
- **Upgrades:** take a backup, replace the binary, run `migrate up`, then
  restart both components. Both always run the same release.
- **Backups:** before every `migrate up`, because a release is rolled back
  by restoring one. The PostgreSQL doc's
  [Day to day](postgres-in-docker.md#day-to-day) shows how for the
  container.

## 10. Clean up

Stop both components with Ctrl-C in their terminals, remove PostgreSQL as
[its doc](postgres-in-docker.md#day-to-day) shows, then delete the
collector, if you started the Quick start's, and the folder:

```sh
# Remove the collector and its image.
docker rm -f opamp-collector
docker rmi opamp-quickstart-collector:dev

# Delete the folder, with the binary, the secrets and the passwords.
rm -rf ~/opamp-standalone
```
