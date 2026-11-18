//nolint:testpackage // checks the unexported DSN and env defaults next to the code
package optout

import (
	"strings"
	"testing"
)

func TestDSNUsesShortTimeoutsAndUTF8MB4(t *testing.T) {
	cfg := Config{Host: "mysql.example.test", Port: "3306", User: "leitor", Password: "p@ss:w/rd", Database: "trentin"}
	dsn := cfg.DSN()

	for _, want := range []string{"leitor:p@ss:w/rd@tcp(mysql.example.test:3306)/trentin", "charset=utf8mb4", "timeout=5s", "readTimeout=5s"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("dsn %q has no %q", dsn, want)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvHost, " db.example.test ")
	t.Setenv(EnvPort, "")
	t.Setenv(EnvUser, "leitor")
	t.Setenv(EnvPassword, "x")
	t.Setenv(EnvDatabase, "trentin")

	cfg, ok := ConfigFromEnv()
	if !ok || cfg.Host != "db.example.test" || cfg.Port != "3306" {
		t.Fatalf("cfg %+v ok %v", cfg, ok)
	}

	t.Setenv(EnvUser, "")

	if _, ok := ConfigFromEnv(); ok {
		t.Fatal("config without user accepted")
	}
}

func TestUnsubscribeBaseFromEnv(t *testing.T) {
	t.Setenv(EnvUnsubscribeBase, "")

	if got := UnsubscribeBaseFromEnv(); got != DefaultUnsubscribeBase {
		t.Fatalf("default %s", got)
	}

	t.Setenv(EnvUnsubscribeBase, "https://example.test/sair")

	if got := UnsubscribeBaseFromEnv(); got != "https://example.test/sair" {
		t.Fatalf("env %s", got)
	}
}
