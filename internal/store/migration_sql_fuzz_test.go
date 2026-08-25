package store

import (
	"strings"
	"testing"
)

const maxFuzzMigrationSQLInput = 128 << 10

func FuzzParseNonTransactionalMigration(f *testing.F) {
	for _, content := range []string{
		`CREATE INDEX CONCURRENTLY messages_channel_idx ON public.messages (channel_id);`,
		`CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS "Message ""Key""" ON "App"."Messages" USING btree ("channel_id" DESC NULLS LAST, (lower("body"))) INCLUDE ("version") WITH (fillfactor = 80) TABLESPACE "Fast Space" WHERE "body" <> E'';`,
		"/* outer /* nested */ still outer */ CREATE INDEX CONCURRENTLY idx ON messages (payload = $$a;b$$);",
		`CREATE INDEX CONCURRENTLY idx ON messages ((payload = E'quote\';semicolon;'));`,
		"CREATE INDEX CONCURRENTLY one ON messages (id); CREATE INDEX CONCURRENTLY two ON messages (id);",
		"CREATE INDEX CONCURRENTLY one ON messages (id);;",
		`CREATE INDEX CONCURRENTLY "unterminated ON messages (id);`,
		"CREATE INDEX CONCURRENTLY idx ON messages ((payload = $tag$unterminated));",
		"CREATE INDEX CONCURRENTLY idx ON messages (id) /* outer /* inner */;",
		"CREATE INDEX CONCURRENTLY idx ON messages (\xff);",
	} {
		f.Add(content)
	}

	f.Fuzz(func(t *testing.T, content string) {
		if len(content) > maxFuzzMigrationSQLInput {
			t.Skip()
		}

		spec, err := parseNonTransactionalMigration(content)
		again, againErr := parseNonTransactionalMigration(content)
		if (err == nil) != (againErr == nil) {
			t.Fatalf("parse success changed between identical inputs: first=%v second=%v", err, againErr)
		}
		if err != nil {
			if err.Error() != againErr.Error() {
				t.Fatalf("parse error changed between identical inputs: first=%q second=%q", err, againErr)
			}
			return
		}
		if spec != again {
			t.Fatalf("parse result changed between identical inputs: first=%#v second=%#v", spec, again)
		}

		if strings.TrimSpace(spec.statement) == "" || strings.TrimSpace(spec.probeSuffix) == "" {
			t.Fatalf("successful parse returned incomplete statement data: %#v", spec)
		}
		if !validConcurrentIndexName(spec.index.name) || !validConcurrentIndexName(spec.table.name) {
			t.Fatalf("successful parse returned invalid identifiers: index=%q table=%q", spec.index.name, spec.table.name)
		}
		if spec.index.schema != "" && !validConcurrentIndexName(spec.index.schema) {
			t.Fatalf("successful parse returned invalid index schema %q", spec.index.schema)
		}
		if spec.table.schema != "" && !validConcurrentIndexName(spec.table.schema) {
			t.Fatalf("successful parse returned invalid table schema %q", spec.table.schema)
		}

		reparsed, reparseErr := parseNonTransactionalMigration(spec.statement)
		if reparseErr != nil {
			t.Fatalf("reparse successful statement: %v", reparseErr)
		}
		if reparsed != spec {
			t.Fatalf("reparse result = %#v, want %#v", reparsed, spec)
		}
	})
}
