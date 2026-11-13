package management

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/telflo/opamp-server/store/database/databasetest"
	"github.com/telflo/opamp-server/store/organizations"
	"github.com/telflo/opamp-server/store/publications"
)

// organizationsServer is a Server over the Postgres stores on d, with no
// organization registered.
func organizationsServer(t *testing.T, d *databasetest.Databases, registry organizations.Store) (*Server, *testClock) {
	t.Helper()
	s, clock, _ := newServer(t, registry, publications.NewPostgres(d.Runtime))
	return s, clock
}

// organizationRows is every row of the registry, every column as text, read
// as the superuser.
func organizationRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select o::text from opamp_server.organizations o order by id`)
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

// postWhileLocked holds a lock every insert into the registry waits for
// (SHARE conflicts with an insert's ROW EXCLUSIVE, not with a read), sends
// the same POST twice at once, waits until both are blocked on the lock in
// pg_stat_activity, and commits: both inserts then run at once, every
// time. It returns both answers.
func postWhileLocked(t *testing.T, s *Server, d *databasetest.Databases, payload string) []*httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	lock, err := d.Admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(ctx) }()
	if _, err := lock.Exec(ctx, "lock table opamp_server.organizations in share mode"); err != nil {
		t.Fatal(err)
	}

	answers := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { answers <- postOrganization(s, payload) }()
	}
	// Bounded by attempts, not a fixed wait: each one sleeps briefly.
	blocked := 0
	for attempt := 0; attempt < 500 && blocked < 2; attempt++ {
		if err := d.Admin.QueryRow(ctx, `
			select count(*) from pg_stat_activity
			 where datname = current_database()
			   and usename = 'telflo_opamp_server'
			   and wait_event_type = 'Lock'
			   and query like '%insert into opamp_server.organizations%'`).Scan(&blocked); err != nil {
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

	var got []*httptest.ResponseRecorder
	for range 2 {
		select {
		case answer := <-answers:
			got = append(got, answer)
		case <-time.After(30 * time.Second):
			t.Fatal("a POST never answered")
		}
	}
	return got
}

// Two simultaneous POSTs of the same new id and name: one 201, one 200, the
// same body, one row. The loser's insert waits on the winner's, and `on
// conflict do nothing` lets it find the same name.
func TestTwoSimultaneousIdenticalPostsAreOneCreatedAndOneUnchanged(t *testing.T) {
	d := testDB(t)
	s, _ := organizationsServer(t, d, organizations.NewPostgres(d.Runtime))

	answers := postWhileLocked(t, s, d, createBody(orgA, "Ana's workspace"))
	statuses := []int{answers[0].Code, answers[1].Code}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusOK, http.StatusCreated}) {
		t.Fatalf("the POSTs answered %v (%s, %s), want one 201 and one 200", statuses, answers[0].Body.String(), answers[1].Body.String())
	}
	want := `{"organizationId":"` + orgA + `"}`
	for _, answer := range answers {
		if answer.Body.String() != want {
			t.Fatalf("a POST answered %s, want %s", answer.Body.String(), want)
		}
	}
	if rows := organizationRows(t, d.Admin); len(rows) != 1 {
		t.Fatalf("%d rows, want 1: %v", len(rows), rows)
	}
}

// The deliberate break for the test above, run every time: a registry that
// reads before it inserts (both POSTs see no row, then both insert) is
// answered 500 under the same lock, so the test above can tell.
func TestTheConcurrencyTestSeesAReadThenInsertRegistry(t *testing.T) {
	d := testDB(t)
	s, _ := organizationsServer(t, d, readThenInsertOrganizations{organizations.NewPostgres(d.Runtime), d.Runtime})

	answers := postWhileLocked(t, s, d, createBody(orgA, "Ana's workspace"))
	statuses := []int{answers[0].Code, answers[1].Code}
	slices.Sort(statuses)
	if !slices.Equal(statuses, []int{http.StatusCreated, http.StatusInternalServerError}) {
		t.Fatalf("the read-then-insert registry answered %v, want 201 and 500", statuses)
	}
}

// readThenInsertOrganizations is the broken shape: it looks for the id
// first, and inserts with a plain insert when it finds none.
type readThenInsertOrganizations struct {
	organizations.Store
	pool *pgxpool.Pool
}

func (r readThenInsertOrganizations) Create(ctx context.Context, o organizations.Organization) (organizations.CreateResult, organizations.Organization, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx,
		`select exists (select 1 from opamp_server.organizations where id = $1)`, o.ID).Scan(&exists); err != nil {
		return 0, organizations.Organization{}, err
	}
	if exists {
		return r.Store.Create(ctx, o)
	}
	if _, err := r.pool.Exec(ctx, `
		insert into opamp_server.organizations (id, name, created_at, updated_at, generation)
		values ($1, $2, $3, $4, gen_random_uuid())`, o.ID, o.Name, o.CreatedAt, o.UpdatedAt); err != nil {
		return 0, organizations.Organization{}, fmt.Errorf("insert: %w", err)
	}
	return organizations.Created, o, nil
}

// created_at and updated_at come from the server's injected clock, to the
// millisecond, never the database's now(): a retried POST later leaves
// both, a rename moves updated_at only.
func TestOrganizationTimesAreTheInjectedClocks(t *testing.T) {
	d := testDB(t)
	s, clock := organizationsServer(t, d, organizations.NewPostgres(d.Runtime))
	ctx := context.Background()
	stored := func() (createdAt, updatedAt time.Time) {
		t.Helper()
		if err := d.Admin.QueryRow(ctx,
			`select created_at, updated_at from opamp_server.organizations where id = $1`, orgA).Scan(&createdAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		return createdAt.UTC(), updatedAt.UTC()
	}
	start := time.Date(2026, 10, 3, 15, 0, 0, 123000000, time.UTC) // the clock's start, to the ms

	expectStatus(t, "POST", postOrganization(s, createBody(orgA, "A")), http.StatusCreated)
	clock.Advance(24 * time.Hour)
	expectStatus(t, "the same POST", postOrganization(s, createBody(orgA, "A")), http.StatusOK)
	if createdAt, updatedAt := stored(); !createdAt.Equal(start) || !updatedAt.Equal(start) {
		t.Fatalf("created_at %s, updated_at %s; want both the test clock's %s", createdAt, updatedAt, start)
	}
	expectStatus(t, "PATCH", call(s, http.MethodPatch, organizationTarget(orgA), []byte(nameBody("B")), jsonType), http.StatusOK)
	if createdAt, updatedAt := stored(); !createdAt.Equal(start) || !updatedAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("after the rename, created_at %s, updated_at %s; want %s and %s", createdAt, updatedAt, start, start.Add(24*time.Hour))
	}
}

// A POST under an organization registered by the platform's id, then that
// organization deleted on the database: the POST's publication goes with it,
// on the real foreign key, and the next POST is refused.
func TestTheForeignKeyCascadesAndRefusesOnTheDatabase(t *testing.T) {
	d := testDB(t)
	s, _ := organizationsServer(t, d, organizations.NewPostgres(d.Runtime))
	expectStatus(t, "POST", postOrganization(s, createBody(orgA, "A")), http.StatusCreated)
	expectStatus(t, "POST", publish(s, orgA, publicationA, validBody(sampleYAML)), http.StatusCreated)
	if rows := tableSnapshot(t, d.Admin); len(rows) != 1 {
		t.Fatalf("%d publications, want 1", len(rows))
	}
	expectStatus(t, "DELETE", call(s, http.MethodDelete, organizationTarget(orgA), nil, ""), http.StatusNoContent)
	if rows := tableSnapshot(t, d.Admin); len(rows) != 0 {
		t.Fatalf("%d publications after the organization's delete, want 0", len(rows))
	}
	at := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if _, _, err := publications.NewPostgres(d.Runtime).Put(context.Background(), publications.Publication{
		ID: uuid.New(), OrganizationID: uuid.MustParse(orgA), ConfigurationID: uuid.New(), ConfigurationName: "c",
		VersionID: uuid.New(), VersionNumber: 1, YAML: "a", SHA256: sha256Hex("a"),
		PublishedAt: at, ReceivedAt: at,
		Collector: publications.Collector{Distribution: sampleDistribution, Version: sampleCollectorVersion},
	}); !errors.Is(err, publications.ErrOrganizationNotFound) {
		t.Fatalf("a Put under the deleted organization = %v, want ErrOrganizationNotFound", err)
	}
}
