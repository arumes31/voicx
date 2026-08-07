package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
)

const (
	indexProbeTable = "voicx_migration_index_probe"
	indexProbeName  = "voicx_migration_index_probe_idx"
)

type resolvedTable struct {
	oid          int64
	schema, name string
}

type indexFingerprint struct {
	unique, nullsNotDistinct bool
	method                   string
	keyCount                 int
	elements                 []string
	predicate                string
	options                  []string
	tablespace               string
}

type expectedConcurrentIndex struct {
	spec         concurrentIndexSpec
	table        resolvedTable
	indexSchema  string
	fingerprint  indexFingerprint
	alreadyValid bool
}

type catalogIndex struct {
	oid, tableOID      int64
	valid, ready, live bool
}

func prepareConcurrentIndex(
	ctx context.Context,
	execer migrationExecer,
	spec concurrentIndexSpec,
) (expectedConcurrentIndex, error) {
	expected, err := buildExpectedConcurrentIndex(ctx, execer, spec)
	if err != nil {
		return expectedConcurrentIndex{}, err
	}

	actual, exists, err := findExactIndex(ctx, execer, expected.indexSchema, spec.index.name)
	if err != nil {
		return expectedConcurrentIndex{}, err
	}
	if !exists {
		return expected, nil
	}
	if actual.tableOID != expected.table.oid {
		return expectedConcurrentIndex{}, fmt.Errorf(
			"concurrent index %s.%s belongs to table OID %d, want %s.%s (OID %d)",
			expected.indexSchema, spec.index.name, actual.tableOID,
			expected.table.schema, expected.table.name, expected.table.oid,
		)
	}
	if !actual.valid {
		// Only an invalid index with the exact schema, name, and target-table
		// identity is an interrupted creation that this runner may remove.
		dropStatement := "DROP INDEX CONCURRENTLY " +
			pq.QuoteIdentifier(expected.indexSchema) + "." + pq.QuoteIdentifier(spec.index.name)
		if _, err := execer.ExecContext(ctx, dropStatement); err != nil { // #nosec G202 -- both identifiers are parsed then quoted
			return expectedConcurrentIndex{}, fmt.Errorf(
				"dropping invalid concurrent index %s.%s: %w",
				expected.indexSchema, spec.index.name, err,
			)
		}
		return expected, nil
	}
	if !actual.ready || !actual.live {
		return expectedConcurrentIndex{}, fmt.Errorf(
			"concurrent index %s.%s is valid but not ready/live",
			expected.indexSchema, spec.index.name,
		)
	}
	actualFingerprint, err := readIndexFingerprint(ctx, execer, actual.oid)
	if err != nil {
		return expectedConcurrentIndex{}, err
	}
	if difference := compareIndexFingerprints(expected.fingerprint, actualFingerprint); difference != "" {
		return expectedConcurrentIndex{}, fmt.Errorf(
			"concurrent index %s.%s has the wrong definition: %s",
			expected.indexSchema, spec.index.name, difference,
		)
	}
	expected.alreadyValid = true
	return expected, nil
}

func verifyConcurrentIndex(
	ctx context.Context,
	execer migrationExecer,
	expected expectedConcurrentIndex,
) error {
	actual, exists, err := findExactIndex(
		ctx, execer, expected.indexSchema, expected.spec.index.name,
	)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf(
			"concurrent index %s.%s is missing after creation",
			expected.indexSchema, expected.spec.index.name,
		)
	}
	if actual.tableOID != expected.table.oid {
		return fmt.Errorf(
			"concurrent index %s.%s targets table OID %d after creation, want %d",
			expected.indexSchema, expected.spec.index.name, actual.tableOID, expected.table.oid,
		)
	}
	if !actual.valid || !actual.ready || !actual.live {
		return fmt.Errorf(
			"concurrent index %s.%s is invalid or not ready/live after creation",
			expected.indexSchema, expected.spec.index.name,
		)
	}
	actualFingerprint, err := readIndexFingerprint(ctx, execer, actual.oid)
	if err != nil {
		return err
	}
	if difference := compareIndexFingerprints(expected.fingerprint, actualFingerprint); difference != "" {
		return fmt.Errorf(
			"concurrent index %s.%s has the wrong definition after creation: %s",
			expected.indexSchema, expected.spec.index.name, difference,
		)
	}
	return nil
}

func buildExpectedConcurrentIndex(
	ctx context.Context,
	execer migrationExecer,
	spec concurrentIndexSpec,
) (expected expectedConcurrentIndex, retErr error) {
	table, err := resolveConcurrentIndexTable(ctx, execer, spec.table)
	if err != nil {
		return expectedConcurrentIndex{}, err
	}
	indexSchema := table.schema
	if spec.index.schema != "" && spec.index.schema != indexSchema {
		return expectedConcurrentIndex{}, fmt.Errorf(
			"concurrent index schema %q differs from target table schema %q",
			spec.index.schema, indexSchema,
		)
	}

	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := execer.ExecContext(cleanupCtx,
			"DROP TABLE IF EXISTS pg_temp."+pq.QuoteIdentifier(indexProbeTable)) // #nosec G202 -- fixed quoted identifier
		if err != nil {
			return fmt.Errorf("dropping concurrent-index probe table: %w", err)
		}
		return nil
	}
	defer func() { retErr = errors.Join(retErr, cleanup()) }()
	if err := cleanup(); err != nil {
		return expectedConcurrentIndex{}, err
	}

	createProbeTable := "CREATE TEMP TABLE " + pq.QuoteIdentifier(indexProbeTable) +
		" (LIKE " + pq.QuoteIdentifier(table.schema) + "." + pq.QuoteIdentifier(table.name) + ")"
	if _, err := execer.ExecContext(ctx, createProbeTable); err != nil { // #nosec G202 -- catalog identifiers are quoted
		return expectedConcurrentIndex{}, fmt.Errorf("creating concurrent-index probe table: %w", err)
	}
	createProbeIndex := "CREATE "
	if spec.unique {
		createProbeIndex += "UNIQUE "
	}
	createProbeIndex += "INDEX " + pq.QuoteIdentifier(indexProbeName) +
		" ON pg_temp." + pq.QuoteIdentifier(indexProbeTable) + " " + spec.probeSuffix
	if _, err := execer.ExecContext(ctx, createProbeIndex); err != nil { // #nosec G202 -- parsed embedded definition on isolated temp table
		return expectedConcurrentIndex{}, fmt.Errorf("creating expected concurrent-index probe: %w", err)
	}

	var probeOID int64
	if err := execer.QueryRowContext(ctx, `SELECT oid::bigint
		FROM pg_catalog.pg_class
		WHERE relnamespace = pg_catalog.pg_my_temp_schema()
		  AND relname = $1
		  AND relkind IN ('i', 'I')`, indexProbeName).Scan(&probeOID); err != nil {
		return expectedConcurrentIndex{}, fmt.Errorf("finding concurrent-index probe: %w", err)
	}
	fingerprint, err := readIndexFingerprint(ctx, execer, probeOID)
	if err != nil {
		return expectedConcurrentIndex{}, fmt.Errorf("reading expected concurrent-index definition: %w", err)
	}
	// Temporary relations may use a configured temp tablespace. The embedded
	// statement's explicit (or absent) TABLESPACE clause is the expectation.
	fingerprint.tablespace = spec.tablespaceName
	return expectedConcurrentIndex{
		spec:        spec,
		table:       table,
		indexSchema: indexSchema,
		fingerprint: fingerprint,
	}, nil
}

func resolveConcurrentIndexTable(
	ctx context.Context,
	execer migrationExecer,
	identity qualifiedIdentifier,
) (resolvedTable, error) {
	lookup := pq.QuoteIdentifier(identity.name)
	if identity.schema != "" {
		lookup = pq.QuoteIdentifier(identity.schema) + "." + lookup
	}
	var table resolvedTable
	if err := execer.QueryRowContext(ctx, `SELECT c.oid::bigint, n.nspname, c.relname
		FROM pg_catalog.pg_class AS c
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		WHERE c.oid = pg_catalog.to_regclass($1)
		  AND c.relkind IN ('r', 'p', 'm')`, lookup).Scan(
		&table.oid, &table.schema, &table.name,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resolvedTable{}, fmt.Errorf("concurrent-index target table %s does not exist", lookup)
		}
		return resolvedTable{}, fmt.Errorf("resolving concurrent-index target table %s: %w", lookup, err)
	}
	return table, nil
}

func findExactIndex(
	ctx context.Context,
	execer migrationExecer,
	schema, name string,
) (catalogIndex, bool, error) {
	var index catalogIndex
	err := execer.QueryRowContext(ctx, `SELECT c.oid::bigint, i.indrelid::bigint,
		       i.indisvalid, i.indisready, i.indislive
		FROM pg_catalog.pg_class AS c
		JOIN pg_catalog.pg_namespace AS n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_index AS i ON i.indexrelid = c.oid
		WHERE n.nspname = $1
		  AND c.relname = $2
		  AND c.relkind IN ('i', 'I')`, schema, name).Scan(
		&index.oid, &index.tableOID, &index.valid, &index.ready, &index.live,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return catalogIndex{}, false, nil
	}
	if err != nil {
		return catalogIndex{}, false, fmt.Errorf("finding concurrent index %s.%s: %w", schema, name, err)
	}
	return index, true, nil
}

func readIndexFingerprint(
	ctx context.Context,
	execer migrationExecer,
	indexOID int64,
) (indexFingerprint, error) {
	var (
		fingerprint  indexFingerprint
		elementCount int
		options      pq.StringArray
	)
	if err := execer.QueryRowContext(ctx, `SELECT i.indisunique, i.indnullsnotdistinct,
		       am.amname, i.indnkeyatts, i.indnatts,
		       COALESCE(pg_catalog.pg_get_expr(i.indpred, i.indrelid, false), ''),
		       COALESCE(c.reloptions, ARRAY[]::text[]),
		       COALESCE(ts.spcname, '')
		FROM pg_catalog.pg_index AS i
		JOIN pg_catalog.pg_class AS c ON c.oid = i.indexrelid
		JOIN pg_catalog.pg_am AS am ON am.oid = c.relam
		LEFT JOIN pg_catalog.pg_tablespace AS ts ON ts.oid = c.reltablespace
		WHERE i.indexrelid = $1::oid`, indexOID).Scan(
		&fingerprint.unique,
		&fingerprint.nullsNotDistinct,
		&fingerprint.method,
		&fingerprint.keyCount,
		&elementCount,
		&fingerprint.predicate,
		&options,
		&fingerprint.tablespace,
	); err != nil {
		return indexFingerprint{}, fmt.Errorf("reading index catalog definition for OID %d: %w", indexOID, err)
	}
	fingerprint.options = append([]string(nil), options...)
	sort.Strings(fingerprint.options)
	fingerprint.elements = make([]string, 0, elementCount)
	for position := 1; position <= elementCount; position++ {
		var element string
		if err := execer.QueryRowContext(ctx,
			`SELECT pg_catalog.pg_get_indexdef($1::oid, $2, false)`, indexOID, position).Scan(&element); err != nil {
			return indexFingerprint{}, fmt.Errorf(
				"reading index element %d for OID %d: %w", position, indexOID, err,
			)
		}
		fingerprint.elements = append(fingerprint.elements, strings.TrimSpace(element))
	}
	return fingerprint, nil
}

func compareIndexFingerprints(want, got indexFingerprint) string {
	switch {
	case want.unique != got.unique:
		return fmt.Sprintf("unique = %t, want %t", got.unique, want.unique)
	case want.nullsNotDistinct != got.nullsNotDistinct:
		return fmt.Sprintf("NULLS NOT DISTINCT = %t, want %t", got.nullsNotDistinct, want.nullsNotDistinct)
	case want.method != got.method:
		return fmt.Sprintf("access method = %q, want %q", got.method, want.method)
	case want.keyCount != got.keyCount:
		return fmt.Sprintf("key count = %d, want %d", got.keyCount, want.keyCount)
	case !slices.Equal(want.elements, got.elements):
		return fmt.Sprintf("ordered key/INCLUDE elements = %q, want %q", got.elements, want.elements)
	case want.predicate != got.predicate:
		return fmt.Sprintf("predicate = %q, want %q", got.predicate, want.predicate)
	case !slices.Equal(want.options, got.options):
		return fmt.Sprintf("storage options = %q, want %q", got.options, want.options)
	case want.tablespace != got.tablespace:
		return fmt.Sprintf("tablespace = %q, want %q", got.tablespace, want.tablespace)
	default:
		return ""
	}
}
