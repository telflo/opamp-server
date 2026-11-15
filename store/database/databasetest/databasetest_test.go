package databasetest

import (
	"context"
	"strings"
	"testing"
)

func TestPrepareRefusesADatabaseWhoseNameDoesNotEndInTest(t *testing.T) {
	// Port 1 has no server: a refusal that tried to connect first would
	// report a connection error instead.
	const unreachable = "postgresql://nobody:nothing@127.0.0.1:1/postgres"
	for _, name := range []string{"telflo_opamp_server", "telflo", "telflo_opamp_server_test_backup", "_test", ""} {
		_, err := Prepare(context.Background(), unreachable, name, true)
		if err == nil || !strings.Contains(err.Error(), `must end in "_test"`) {
			t.Errorf("Prepare(%q) = %v, want a refusal naming the _test suffix", name, err)
		}
	}
}

func TestTemplateNameIsATestDatabaseOfItsOwn(t *testing.T) {
	got := TemplateName("telflo_opamp_server_collectors_test")
	if got != "telflo_opamp_server_collectors_template_test" {
		t.Fatalf("TemplateName() = %q", got)
	}
	if err := CheckName(got); err != nil {
		t.Fatal(err)
	}
}

func TestCheckNameAcceptsATestDatabase(t *testing.T) {
	if err := CheckName("telflo_opamp_server_database_test"); err != nil {
		t.Fatal(err)
	}
}

func TestURLForReplacesTheUserPasswordAndDatabase(t *testing.T) {
	got, err := URLFor("postgresql://admin:secret@localhost:55432/postgres?sslmode=disable",
		"telflo_opamp_server", "telflo-opamp-local", "telflo_opamp_server_x_test")
	if err != nil {
		t.Fatal(err)
	}
	want := "postgresql://telflo_opamp_server:telflo-opamp-local@localhost:55432/telflo_opamp_server_x_test?sslmode=disable"
	if got != want {
		t.Fatalf("URLFor() = %q, want %q", got, want)
	}
}

func TestAnAdminURLThatIsNotAURLIsRefusedWithoutQuotingIt(t *testing.T) {
	_, err := URLFor("host=localhost user=admin password=secret", "u", "p", "d_test")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("URLFor() = %v, want a refusal that doesn't quote the setting", err)
	}
}
