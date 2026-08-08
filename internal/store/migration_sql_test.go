package store

import (
	"strings"
	"testing"
)

func TestSplitPostgreSQLStatementsHonorsLexicalStates(t *testing.T) {
	source := `SELECT
    'regular;''quoted',
    E'escape\';still in string',
    "quoted;""identifier",
    $$dollar;--not a comment/*nor this*/$$,
    $tag_1$tagged;string$tag_1$
/* outer; /* nested; */ still outer; */
-- line comment;
;`
	statements, err := splitPostgreSQLStatements(source)
	if err != nil {
		t.Fatalf("splitPostgreSQLStatements: %v", err)
	}
	if len(statements) != 1 {
		t.Fatalf("splitPostgreSQLStatements returned %d statements, want 1: %q", len(statements), statements)
	}
	if !strings.Contains(statements[0], "$tag_1$tagged;string$tag_1$") {
		t.Fatalf("statement lost tagged dollar quote: %q", statements[0])
	}
}

func TestParseNonTransactionalMigration(t *testing.T) {
	content := `-- voicx:no-transaction
CREATE/* comment is whitespace */UNIQUE
INDEX/* outer /* nested */ comment */CONCURRENTLY IF NOT EXISTS "Message ""Key"""
ON ONLY "App"."Messages" USING btree
("channel_id" DESC NULLS LAST, (lower("body"))) INCLUDE ("version")
WITH (fillfactor = 80) TABLESPACE "Fast Space"
WHERE "body" <> E'';`
	spec, err := parseNonTransactionalMigration(content)
	if err != nil {
		t.Fatalf("parseNonTransactionalMigration: %v", err)
	}
	if !spec.unique {
		t.Fatal("unique = false, want true")
	}
	if spec.index != (qualifiedIdentifier{name: `Message "Key"`}) {
		t.Fatalf("index identity = %#v", spec.index)
	}
	if spec.table != (qualifiedIdentifier{schema: "App", name: "Messages"}) {
		t.Fatalf("table identity = %#v", spec.table)
	}
	if spec.tablespaceName != "Fast Space" {
		t.Fatalf("tablespace = %q, want %q", spec.tablespaceName, "Fast Space")
	}
	if strings.Contains(strings.ToLower(spec.probeSuffix), "tablespace") {
		t.Fatalf("probe suffix retained TABLESPACE: %q", spec.probeSuffix)
	}
	if !strings.HasPrefix(strings.ToLower(spec.probeSuffix), "using btree") {
		t.Fatalf("probe suffix lost access method: %q", spec.probeSuffix)
	}
	if !strings.Contains(strings.ToLower(spec.probeSuffix), `where "body" <> e''`) {
		t.Fatalf("probe suffix lost predicate: %q", spec.probeSuffix)
	}
}

func TestParseNonTransactionalMigrationFailsClosed(t *testing.T) {
	tests := map[string]string{
		"empty":                      "-- comments only\n/* no SQL */",
		"ordinary index":             "CREATE INDEX idx ON messages (id);",
		"not concurrent":             "CREATE UNIQUE INDEX idx ON messages (id);",
		"two statements":             "CREATE INDEX CONCURRENTLY one ON messages (id); CREATE INDEX CONCURRENTLY two ON messages (id);",
		"extra empty statement":      "CREATE INDEX CONCURRENTLY one ON messages (id);;",
		"statement after comment":    "CREATE INDEX CONCURRENTLY one ON messages (id); /* ; */ DROP TABLE messages;",
		"comment keyword splice":     "CRE/* not token whitespace */ATE INDEX CONCURRENTLY one ON messages (id);",
		"overlong identifier":        "CREATE INDEX CONCURRENTLY " + strings.Repeat("a", 64) + " ON messages (id);",
		"three part target":          "CREATE INDEX CONCURRENTLY one ON catalog.public.messages (id);",
		"unterminated regular":       "CREATE INDEX CONCURRENTLY one ON messages ((payload = 'x));",
		"unterminated escape":        `CREATE INDEX CONCURRENTLY one ON messages ((payload = E'x\'));`,
		"unterminated identifier":    `CREATE INDEX CONCURRENTLY "one ON messages (id);`,
		"unterminated dollar quote":  `CREATE INDEX CONCURRENTLY one ON messages (($tag$x));`,
		"unterminated block comment": "CREATE INDEX CONCURRENTLY one ON messages (id) /* outer /* inner */;",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if spec, err := parseNonTransactionalMigration(content); err == nil {
				t.Fatalf("parseNonTransactionalMigration unexpectedly accepted %#v", spec)
			}
		})
	}
}

func TestEveryMarkedMigrationIsOneSupportedStatement(t *testing.T) {
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatalf("loadEmbeddedMigrations: %v", err)
	}
	for _, migration := range migrations {
		if !migration.nonTransactional {
			continue
		}
		t.Run(migration.filename, func(t *testing.T) {
			spec, err := parseNonTransactionalMigration(string(migration.content))
			if err != nil {
				t.Fatalf("marked migration must contain one supported concurrent index: %v", err)
			}
			if spec.table.schema != "public" {
				t.Fatalf("marked migration target schema = %q, want explicit public", spec.table.schema)
			}
		})
	}
}
