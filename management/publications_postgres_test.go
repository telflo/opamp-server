package management

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/telflo-opamp-server/store/database/databasetest"
	"github.com/telflo/telflo-opamp-server/store/organizations"
	"github.com/telflo/telflo-opamp-server/store/publications"
)

// This package's own test database: go test ./... runs packages in
// parallel.
const testDatabase = "telflo_opamp_management_test"

// postgresServer is a Server over the Postgres stores on a fresh, migrated
// database, connected as telflo_opamp (what serve connects as), with
// organizations A and B registered.
func postgresServer(t *testing.T) (*Server, *testClock, *databasetest.Databases) {
	t.Helper()
	d := testDB(t)
	s, clock := postgresServerOver(t, d, publications.NewPostgres(d.Runtime))
	return s, clock, d
}

// postgresServerOver is a Server over store and the Postgres registry on d,
// with organizations A and B registered.
func postgresServerOver(t *testing.T, d *databasetest.Databases, store publications.Store) (*Server, *testClock) {
	t.Helper()
	registry := organizations.NewPostgres(d.Runtime)
	register(t, registry, orgA, orgB)
	s, clock, _ := newServerWith(t, registry, store, postgresFleetStores(d.Runtime))
	return s, clock
}

// tableSnapshot is every row of the table, every column as text, in id
// order, read as the superuser: what "unchanged" is checked against.
func tableSnapshot(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`select p::text from telflo_opamp.publications p order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// organizationSnapshot is organization A's rows, publications and
// registry, every column as text, read as the superuser: what "unchanged"
// is checked against.
func organizationSnapshot(t *testing.T, pool *pgxpool.Pool, organizationID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		select p::text from telflo_opamp.publications p where organization_id = $1
		union all
		select o::text from telflo_opamp.organizations o where id = $1
		order by 1`, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// Every endpoint, called as organization B with A's ids, answers 404
// (reads, deletes) or 409 (puts, and a POST of A's id under another name),
// never returns A's data, and leaves A's rows, its publications and its
// registration, exactly as they were; B's own organization routes (read,
// rename, delete with its publications) touch only B's. On real Postgres,
// as the server's role.
func TestNoEndpointLetsAnotherOrganizationSeeOrChangeARow(t *testing.T) {
	s, _, d := postgresServer(t)
	valid := validBody(trickyYAML)
	expectStatus(t, "A's POST", publish(s, orgA, publicationA, valid), http.StatusCreated)
	second := uuid.NewString()
	expectStatus(t, "A's second POST", publish(s, orgA, second, valid.with("publishedAt", "2026-10-04T00:00:00Z")), http.StatusCreated)
	// B's own publication, of another configuration, that B's delete
	// removes.
	theirs := uuid.NewString()
	expectStatus(t, "B's POST", publish(s, orgB, theirs, validBody(sampleYAML).
		with("configuration.id", configurationB).with("configuration.name", "b-config").with("publishedBy.email", "bo@b.example")),
		http.StatusCreated)
	before := organizationSnapshot(t, d.Admin, orgA)
	if len(before) != 3 {
		t.Fatalf("%d rows, want A's 2 publications and its registration", len(before))
	}

	notFound := `{"error":{"code":"not_found","message":"Not found."}}`
	// anyBody accepts any reply body, for B's own organization, whose
	// times the test doesn't fix; it is still checked for A's data.
	const anyBody = "(any body)"
	cases := []struct {
		name    string
		method  string
		target  string
		payload []byte
		status  int
		body    string
	}{
		{"GET A's publication", http.MethodGet, publicationTarget(orgB, publicationA), nil, 404, notFound},
		{"GET A's other publication", http.MethodGet, publicationTarget(orgB, second), nil, 404, notFound},
		{"DELETE A's publication", http.MethodDelete, publicationTarget(orgB, publicationA), nil, 404, notFound},
		{"POST A's id with A's body", http.MethodPost, listTarget(orgB, ""), valid.with("id", publicationA).bytes(), 409, conflictBody},
		{"POST A's id with another body", http.MethodPost, listTarget(orgB, ""), valid.with("id", publicationA).withYAML("exporters: {}\n").bytes(), 409, conflictBody},
		{"list A's configuration", http.MethodGet, listTarget(orgB, "?configurationId="+configurationA), nil, 200, `{"publications":[],"nextCursor":null}`},
		{"list A's configuration from A's cursor", http.MethodGet,
			listTarget(orgB, "?configurationId="+configurationA+"&limit=1&cursor="+
				(&publications.Cursor{PublishedAt: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), ID: uuid.Max}).String()),
			nil, 200, `{"publications":[],"nextCursor":null}`},
		{"DELETE A's configuration's publications", http.MethodDelete, configurationTarget(orgB, configurationA), nil, 204, ""},
		// The organization routes, with B's own id or A's.
		{"GET B's organization", http.MethodGet, organizationTarget(orgB), nil, 200, anyBody},
		{"PATCH B's organization", http.MethodPatch, organizationTarget(orgB), []byte(nameBody("B, renamed")), 200, anyBody},
		{"POST A's id under another name", http.MethodPost, organizationsPath, []byte(createBody(orgA, "B's idea of A")), 409, organizationConflictBody},
		{"DELETE B's organization", http.MethodDelete, organizationTarget(orgB), nil, 204, ""},
		{"GET A's publication once B is gone", http.MethodGet, publicationTarget(orgB, publicationA), nil, 404, organizationNotFoundBody},
		{"POST A's id once B is gone", http.MethodPost, listTarget(orgB, ""), valid.with("id", publicationA).bytes(), 404, organizationNotFoundBody},
		{"list A's configuration once B is gone", http.MethodGet, listTarget(orgB, "?configurationId="+configurationA), nil, 404, organizationNotFoundBody},
		{"DELETE A's configuration's once B is gone", http.MethodDelete, configurationTarget(orgB, configurationA), nil, 404, organizationNotFoundBody},
	}
	for _, c := range cases {
		got := call(s, c.method, c.target, c.payload, jsonType)
		if got.Code != c.status || (c.body != anyBody && got.Body.String() != c.body) {
			t.Errorf("%s as B: %d %s, want %d %s", c.name, got.Code, got.Body.String(), c.status, c.body)
		}
		for _, leaked := range []string{"payments-gateway", "receivers", "ana@acme.io", orgA, "organization " + orgA} {
			if strings.Contains(got.Body.String(), leaked) {
				t.Errorf("%s as B: the answer holds A's %q", c.name, leaked)
			}
		}
		if after := organizationSnapshot(t, d.Admin, orgA); !slices.Equal(after, before) {
			t.Fatalf("%s as B changed A's rows:\n before %v\n after  %v", c.name, before, after)
		}
	}
	// B's delete took B's publication with it, and nothing of A's.
	if rows := tableSnapshot(t, d.Admin); len(rows) != 2 || slices.ContainsFunc(rows, func(row string) bool { return strings.Contains(row, theirs) }) {
		t.Fatalf("after B's delete, the publications are %v, want A's 2 alone", rows)
	}

	// And A still reads its publication, YAML and all.
	got := call(s, http.MethodGet, publicationTarget(orgA, publicationA), nil, "")
	expectStatus(t, "A's GET", got, http.StatusOK)
	assertReply(t, "A's publication", decodeReply(t, got), expectedReply(trickyYAML))
}

// received_at comes from the server's injected clock, never the database's
// now(), and an identical POST later leaves it.
func TestReceivedAtIsTheInjectedClocksTime(t *testing.T) {
	s, clock, d := postgresServer(t)
	expectStatus(t, "POST", publish(s, orgA, publicationA, validBody(sampleYAML)), http.StatusCreated)
	clock.Advance(24 * time.Hour)
	expectStatus(t, "the identical POST", publish(s, orgA, publicationA, validBody(sampleYAML)), http.StatusOK)

	var receivedAt time.Time
	if err := d.Admin.QueryRow(context.Background(),
		`select received_at from telflo_opamp.publications where organization_id = $1 and id = $2`,
		orgA, publicationA).Scan(&receivedAt); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 10, 3, 15, 0, 0, 123000000, time.UTC) // the clock's start, to the ms
	if !receivedAt.Equal(want) {
		t.Fatalf("received_at = %s, want the test clock's %s", receivedAt.UTC(), want)
	}
}

// Two simultaneous POSTs of the same new id and body: one 201, one 200, one
// row. Deterministic: a separate connection holds a lock that every insert
// into the table waits for (SHARE conflicts with an insert's ROW EXCLUSIVE,
// not with a read), the test starts both POSTs and waits until both are
// blocked on it in pg_stat_activity, then commits. Both inserts then run at
// once: the loser waits on the winner's open insert, and `on conflict do
// nothing` lets it find the identical row. A read-then-insert store, or a
// plain insert, answers the loser 500 here.
func TestTwoSimultaneousIdenticalPublicationPostsAreOneCreatedAndOneIdentical(t *testing.T) {
	s, _, d := postgresServer(t)
	ctx := context.Background()

	lock, err := d.Admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, "lock table telflo_opamp.publications in share mode"); err != nil {
		t.Fatal(err)
	}

	payload := validBody(sampleYAML).with("id", publicationA).bytes()
	answers := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			answers <- call(s, http.MethodPost, listTarget(orgA, ""), payload, jsonType)
		}()
	}

	// Bounded by attempts, not a fixed wait: each one sleeps briefly.
	blocked := 0
	for attempt := 0; attempt < 500 && blocked < 2; attempt++ {
		if err := d.Admin.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			 where datname = current_database()
			   and usename = 'telflo_opamp'
			   and wait_event_type = 'Lock'
			   and query like '%insert into telflo_opamp.publications%'`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked < 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if blocked != 2 {
		t.Fatalf("%d POSTs waited on the lock, want both", blocked)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var statuses []int
	var bodies []string
	for range 2 {
		select {
		case got := <-answers:
			statuses = append(statuses, got.Code)
			bodies = append(bodies, got.Body.String())
		case <-time.After(30 * time.Second):
			t.Fatal("a POST never answered")
		}
	}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusOK, http.StatusCreated}) {
		t.Fatalf("the POSTs answered %v (%v), want one 201 and one 200", statuses, bodies)
	}
	if bodies[0] != bodies[1] {
		t.Fatalf("the two POSTs answered different publications:\n%s\n%s", bodies[0], bodies[1])
	}
	if rows := tableSnapshot(t, d.Admin); len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
}

// The deliberate break for the test above, run every time: a store that
// reads before it inserts (both POSTs see no row, then both insert) is
// answered 500 under the same lock, so the test above can tell.
func TestTheConcurrencyTestSeesAReadThenInsertStore(t *testing.T) {
	d := testDB(t)
	s, _ := postgresServerOver(t, d, readThenInsert{publications.NewPostgres(d.Runtime), d.Runtime})
	ctx := context.Background()

	lock, err := d.Admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, "lock table telflo_opamp.publications in share mode"); err != nil {
		t.Fatal(err)
	}
	payload := validBody(sampleYAML).with("id", publicationA).bytes()
	answers := make(chan int, 2)
	for range 2 {
		go func() {
			answers <- call(s, http.MethodPost, listTarget(orgA, ""), payload, jsonType).Code
		}()
	}
	blocked := 0
	for attempt := 0; attempt < 500 && blocked < 2; attempt++ {
		if err := d.Admin.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			 where datname = current_database() and usename = 'telflo_opamp' and wait_event_type = 'Lock'`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked < 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if blocked != 2 {
		t.Fatalf("%d POSTs waited on the lock, want both", blocked)
	}
	if err := lock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	statuses := []int{<-answers, <-answers}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusCreated, http.StatusInternalServerError}) {
		t.Fatalf("the read-then-insert store answered %v, want 201 and 500", statuses)
	}
}

// readThenInsert is the broken shape: it looks for the id first, and
// inserts with a plain insert when it finds none.
type readThenInsert struct {
	publications.Store
	pool *pgxpool.Pool
}

func (r readThenInsert) Put(ctx context.Context, p publications.Publication) (publications.PutResult, publications.Publication, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`select exists (select 1 from telflo_opamp.publications where id = $1)`, p.ID).Scan(&exists); err != nil {
		return 0, publications.Publication{}, err
	}
	if exists {
		return r.Store.Put(ctx, p)
	}
	var actorID *uuid.UUID
	var actorEmail *string
	if p.PublishedBy != nil {
		actorID, actorEmail = &p.PublishedBy.ID, &p.PublishedBy.Email
	}
	if _, err := r.pool.Exec(ctx, `
		insert into telflo_opamp.publications (id, organization_id, configuration_id, configuration_name,
		  version_id, version_number, version_label, yaml, sha256, published_by_id, published_by_email,
		  published_at, received_at, collector_distribution, collector_version)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
		p.ID, p.OrganizationID, p.ConfigurationID, p.ConfigurationName, p.VersionID, p.VersionNumber,
		p.VersionLabel, p.YAML, p.SHA256, actorID, actorEmail, p.PublishedAt, p.ReceivedAt,
		p.Collector.Distribution, p.Collector.Version); err != nil {
		return 0, publications.Publication{}, fmt.Errorf("insert: %w", err)
	}
	return publications.Created, p, nil
}
