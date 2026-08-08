package store

import (
	"fmt"
	"strings"
	"unicode"
)

type sqlTokenKind uint8

const (
	sqlTokenIdentifier sqlTokenKind = iota
	sqlTokenQuotedIdentifier
	sqlTokenLiteral
	sqlTokenSymbol
)

type sqlToken struct {
	kind       sqlTokenKind
	raw, value string
	start, end int
}

type qualifiedIdentifier struct {
	schema string
	name   string
}

type concurrentIndexSpec struct {
	statement      string
	unique         bool
	index          qualifiedIdentifier
	table          qualifiedIdentifier
	probeSuffix    string
	tablespaceName string
}

// parseNonTransactionalMigration accepts exactly one supported concurrent
// index statement. This deliberately keeps marked files from becoming a path
// around the transactional migration runner.
func parseNonTransactionalMigration(content string) (concurrentIndexSpec, error) {
	statements, err := splitPostgreSQLStatements(content)
	if err != nil {
		return concurrentIndexSpec{}, err
	}
	if len(statements) != 1 {
		return concurrentIndexSpec{}, fmt.Errorf(
			"non-transactional migration contains %d SQL statements, want exactly 1",
			len(statements),
		)
	}
	spec, err := parseConcurrentIndexStatement(statements[0])
	if err != nil {
		return concurrentIndexSpec{}, fmt.Errorf("unsupported non-transactional migration: %w", err)
	}
	return spec, nil
}

// splitPostgreSQLStatements recognizes PostgreSQL quoting and comments before
// treating a semicolon as a statement boundary. Empty statements are rejected
// so extra separators cannot hide unsupported SQL.
func splitPostgreSQLStatements(source string) ([]string, error) {
	statements := make([]string, 0, 1)
	statementStart := 0
	hasSQL := false

	for i := 0; i < len(source); {
		switch {
		case isSQLSpace(source[i]):
			i++
		case strings.HasPrefix(source[i:], "--"):
			i = skipLineComment(source, i+2)
		case strings.HasPrefix(source[i:], "/*"):
			end, err := skipBlockComment(source, i)
			if err != nil {
				return nil, err
			}
			i = end
		case source[i] == '\'':
			hasSQL = true
			end, err := skipSingleQuotedString(source, i, isEscapeStringPrefix(source, i))
			if err != nil {
				return nil, err
			}
			i = end
		case source[i] == '"':
			hasSQL = true
			end, _, err := scanQuotedIdentifier(source, i)
			if err != nil {
				return nil, err
			}
			i = end
		case source[i] == '$':
			delimiter, ok := dollarQuoteDelimiter(source, i)
			if !ok {
				hasSQL = true
				i++
				continue
			}
			hasSQL = true
			end := strings.Index(source[i+len(delimiter):], delimiter)
			if end < 0 {
				return nil, fmt.Errorf("unterminated dollar-quoted string %s", delimiter)
			}
			i += len(delimiter) + end + len(delimiter)
		case source[i] == ';':
			if !hasSQL {
				return nil, fmt.Errorf("empty SQL statement at byte %d", i)
			}
			statements = append(statements, strings.TrimSpace(source[statementStart:i]))
			i++
			statementStart = i
			hasSQL = false
		default:
			hasSQL = true
			i++
		}
	}

	if hasSQL {
		statements = append(statements, strings.TrimSpace(source[statementStart:]))
	}
	return statements, nil
}

func parseConcurrentIndexStatement(statement string) (concurrentIndexSpec, error) {
	tokens, err := lexPostgreSQLTokens(statement)
	if err != nil {
		return concurrentIndexSpec{}, err
	}
	position := 0
	expectKeyword := func(keyword string) error {
		if position >= len(tokens) || !tokens[position].isKeyword(keyword) {
			return fmt.Errorf("expected %s", keyword)
		}
		position++
		return nil
	}

	if err := expectKeyword("create"); err != nil {
		return concurrentIndexSpec{}, err
	}
	unique := false
	if position < len(tokens) && tokens[position].isKeyword("unique") {
		unique = true
		position++
	}
	if err := expectKeyword("index"); err != nil {
		return concurrentIndexSpec{}, err
	}
	if err := expectKeyword("concurrently"); err != nil {
		return concurrentIndexSpec{}, err
	}
	if position < len(tokens) && tokens[position].isKeyword("if") {
		position++
		if err := expectKeyword("not"); err != nil {
			return concurrentIndexSpec{}, err
		}
		if err := expectKeyword("exists"); err != nil {
			return concurrentIndexSpec{}, err
		}
	}

	index, next, err := parseQualifiedIdentifier(tokens, position)
	if err != nil {
		return concurrentIndexSpec{}, fmt.Errorf("index identity: %w", err)
	}
	position = next
	if err := expectKeyword("on"); err != nil {
		return concurrentIndexSpec{}, err
	}
	if position < len(tokens) && tokens[position].isKeyword("only") {
		position++
	}
	table, next, err := parseQualifiedIdentifier(tokens, position)
	if err != nil {
		return concurrentIndexSpec{}, fmt.Errorf("target table identity: %w", err)
	}
	if table.schema == "" {
		return concurrentIndexSpec{}, errorsAtToken(
			tokens, position, "concurrent-index target table must be schema-qualified",
		)
	}
	position = next
	suffixPosition := position

	if position < len(tokens) && tokens[position].isKeyword("using") {
		position++
		if position >= len(tokens) || !tokens[position].isIdentifier() {
			return concurrentIndexSpec{}, errorsAtToken(tokens, position, "expected index access method")
		}
		position++
	}
	if position >= len(tokens) || tokens[position].raw != "(" {
		return concurrentIndexSpec{}, errorsAtToken(tokens, position, "expected index key list")
	}

	probeSuffix, tablespaceName, err := probeSuffixAndTablespace(statement, tokens, suffixPosition)
	if err != nil {
		return concurrentIndexSpec{}, err
	}
	if strings.TrimSpace(probeSuffix) == "" || tokens[suffixPosition].start >= len(statement) {
		return concurrentIndexSpec{}, errorsAtToken(tokens, position, "empty index definition")
	}
	return concurrentIndexSpec{
		statement:      statement,
		unique:         unique,
		index:          index,
		table:          table,
		probeSuffix:    probeSuffix,
		tablespaceName: tablespaceName,
	}, nil
}

func lexPostgreSQLTokens(source string) ([]sqlToken, error) {
	tokens := make([]sqlToken, 0, 32)
	for i := 0; i < len(source); {
		switch {
		case isSQLSpace(source[i]):
			i++
		case strings.HasPrefix(source[i:], "--"):
			i = skipLineComment(source, i+2)
		case strings.HasPrefix(source[i:], "/*"):
			end, err := skipBlockComment(source, i)
			if err != nil {
				return nil, err
			}
			i = end
		case source[i] == '"':
			end, value, err := scanQuotedIdentifier(source, i)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, sqlToken{
				kind: sqlTokenQuotedIdentifier, raw: source[i:end], value: value, start: i, end: end,
			})
			i = end
		case source[i] == '\'':
			end, err := skipSingleQuotedString(source, i, isEscapeStringPrefix(source, i))
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, sqlToken{
				kind: sqlTokenLiteral, raw: source[i:end], value: source[i:end], start: i, end: end,
			})
			i = end
		case source[i] == '$':
			delimiter, ok := dollarQuoteDelimiter(source, i)
			if !ok {
				tokens = append(tokens, sqlToken{
					kind: sqlTokenSymbol, raw: source[i : i+1], value: source[i : i+1], start: i, end: i + 1,
				})
				i++
				continue
			}
			endOffset := strings.Index(source[i+len(delimiter):], delimiter)
			if endOffset < 0 {
				return nil, fmt.Errorf("unterminated dollar-quoted string %s", delimiter)
			}
			end := i + len(delimiter) + endOffset + len(delimiter)
			tokens = append(tokens, sqlToken{
				kind: sqlTokenLiteral, raw: source[i:end], value: source[i:end], start: i, end: end,
			})
			i = end
		case isIdentifierStart(source[i]):
			start := i
			i++
			for i < len(source) && isIdentifierContinue(source[i]) {
				i++
			}
			raw := source[start:i]
			tokens = append(tokens, sqlToken{
				kind: sqlTokenIdentifier, raw: raw, value: strings.ToLower(raw), start: start, end: i,
			})
		case source[i] == ';':
			return nil, fmt.Errorf("unexpected statement separator at byte %d", i)
		default:
			tokens = append(tokens, sqlToken{
				kind: sqlTokenSymbol, raw: source[i : i+1], value: source[i : i+1], start: i, end: i + 1,
			})
			i++
		}
	}
	return tokens, nil
}

func parseQualifiedIdentifier(tokens []sqlToken, position int) (qualifiedIdentifier, int, error) {
	if position >= len(tokens) || !tokens[position].isIdentifier() {
		return qualifiedIdentifier{}, position, errorsAtToken(tokens, position, "expected identifier")
	}
	first := tokens[position].value
	if !validConcurrentIndexName(first) {
		return qualifiedIdentifier{}, position, fmt.Errorf("invalid identifier %q", first)
	}
	position++
	if position >= len(tokens) || tokens[position].raw != "." {
		return qualifiedIdentifier{name: first}, position, nil
	}
	position++
	if position >= len(tokens) || !tokens[position].isIdentifier() {
		return qualifiedIdentifier{}, position, errorsAtToken(tokens, position, "expected identifier after schema qualifier")
	}
	second := tokens[position].value
	if !validConcurrentIndexName(second) {
		return qualifiedIdentifier{}, position, fmt.Errorf("invalid identifier %q", second)
	}
	position++
	if position < len(tokens) && tokens[position].raw == "." {
		return qualifiedIdentifier{}, position, errorsAtToken(tokens, position, "more than one schema qualifier")
	}
	return qualifiedIdentifier{schema: first, name: second}, position, nil
}

func validConcurrentIndexName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func probeSuffixAndTablespace(
	statement string,
	tokens []sqlToken,
	start int,
) (suffix, tablespace string, err error) {
	depth := 0
	removeStart, removeEnd := -1, -1
	for position := start; position < len(tokens); position++ {
		token := tokens[position]
		switch token.raw {
		case "(":
			depth++
		case ")":
			depth--
			if depth < 0 {
				return "", "", errorsAtToken(tokens, position, "unbalanced closing parenthesis")
			}
		default:
			if depth != 0 || !token.isKeyword("tablespace") {
				if depth == 0 && token.isKeyword("where") {
					position = len(tokens)
				}
				continue
			}
			if removeStart >= 0 {
				return "", "", errorsAtToken(tokens, position, "multiple TABLESPACE clauses")
			}
			if position+1 >= len(tokens) || !tokens[position+1].isIdentifier() {
				return "", "", errorsAtToken(tokens, position+1, "expected tablespace identifier")
			}
			tablespace = tokens[position+1].value
			removeStart = token.start
			removeEnd = tokens[position+1].end
			position++
		}
	}
	if depth != 0 {
		return "", "", fmt.Errorf("unbalanced index-definition parentheses")
	}

	suffixStart := tokens[start].start
	if removeStart < 0 {
		return strings.TrimSpace(statement[suffixStart:]), "", nil
	}
	return strings.TrimSpace(statement[suffixStart:removeStart] + " " + statement[removeEnd:]), tablespace, nil
}

func (t sqlToken) isIdentifier() bool {
	return t.kind == sqlTokenIdentifier || t.kind == sqlTokenQuotedIdentifier
}

func (t sqlToken) isKeyword(keyword string) bool {
	return t.kind == sqlTokenIdentifier && t.value == keyword
}

func errorsAtToken(tokens []sqlToken, position int, message string) error {
	if position >= len(tokens) {
		return fmt.Errorf("%s at end of statement", message)
	}
	return fmt.Errorf("%s near %q", message, tokens[position].raw)
}

func skipLineComment(source string, position int) int {
	for position < len(source) && source[position] != '\n' {
		position++
	}
	return position
}

func skipBlockComment(source string, position int) (int, error) {
	depth := 1
	position += 2
	for position < len(source) {
		switch {
		case strings.HasPrefix(source[position:], "/*"):
			depth++
			position += 2
		case strings.HasPrefix(source[position:], "*/"):
			depth--
			position += 2
			if depth == 0 {
				return position, nil
			}
		default:
			position++
		}
	}
	return 0, fmt.Errorf("unterminated block comment")
}

func skipSingleQuotedString(source string, position int, escapeBackslash bool) (int, error) {
	position++
	for position < len(source) {
		if escapeBackslash && source[position] == '\\' {
			position += 2
			continue
		}
		if source[position] != '\'' {
			position++
			continue
		}
		if position+1 < len(source) && source[position+1] == '\'' {
			position += 2
			continue
		}
		return position + 1, nil
	}
	return 0, fmt.Errorf("unterminated single-quoted string")
}

func scanQuotedIdentifier(source string, position int) (end int, value string, err error) {
	var decoded strings.Builder
	position++
	for position < len(source) {
		if source[position] != '"' {
			decoded.WriteByte(source[position])
			position++
			continue
		}
		if position+1 < len(source) && source[position+1] == '"' {
			decoded.WriteByte('"')
			position += 2
			continue
		}
		return position + 1, decoded.String(), nil
	}
	return 0, "", fmt.Errorf("unterminated quoted identifier")
}

func dollarQuoteDelimiter(source string, position int) (string, bool) {
	if position > 0 && isIdentifierContinue(source[position-1]) {
		return "", false
	}
	end := position + 1
	if end < len(source) && source[end] == '$' {
		return "$$", true
	}
	if end >= len(source) || !isIdentifierStart(source[end]) {
		return "", false
	}
	end++
	for end < len(source) && isIdentifierContinueWithoutDollar(source[end]) {
		end++
	}
	if end >= len(source) || source[end] != '$' {
		return "", false
	}
	return source[position : end+1], true
}

func isEscapeStringPrefix(source string, quotePosition int) bool {
	if quotePosition == 0 || (source[quotePosition-1] != 'e' && source[quotePosition-1] != 'E') {
		return false
	}
	return quotePosition == 1 || !isIdentifierContinue(source[quotePosition-2])
}

func isSQLSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f'
}

func isIdentifierStart(b byte) bool {
	return b == '_' || b >= utf8RuneSelf || unicode.IsLetter(rune(b))
}

func isIdentifierContinue(b byte) bool {
	return isIdentifierContinueWithoutDollar(b) || b == '$'
}

func isIdentifierContinueWithoutDollar(b byte) bool {
	return isIdentifierStart(b) || b >= '0' && b <= '9'
}

// utf8RuneSelf avoids importing unicode/utf8 for its single-byte boundary.
const utf8RuneSelf = 0x80
