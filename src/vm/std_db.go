package vm

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	. "language.com/src/tinyerrors"
)

// The db module is a thin, driver-agnostic layer over Go's database/sql,
// modelled on the bun:sqlite API plus a couple of sqlx ergonomics (named
// parameters and placeholder rebinding). Three pure-Go drivers are wired so
// the CGO_ENABLED=0 build keeps working.
type dbDriver struct {
	sqlName  string // driver name registered with database/sql
	postgres bool   // uses $N placeholders instead of ?
}

var dbDrivers = map[string]dbDriver{
	"sqlite":   {sqlName: "sqlite"},
	"postgres": {sqlName: "pgx", postgres: true},
	"postgresql": {sqlName: "pgx", postgres: true},
	"mysql":    {sqlName: "mysql"},
}

// queryExec is implemented by both *sql.DB and *sql.Tx, so the row/exec
// helpers work the same inside or outside a transaction.
type queryExec interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
}

type NativeDBValue struct {
	db     *sql.DB
	driver dbDriver
	dsn    string
	closed bool
}

type NativeTxValue struct {
	tx     *sql.Tx
	driver dbDriver
	done   bool
}

type NativeStmtValue struct {
	stmt   *sql.Stmt
	driver dbDriver
	closed bool
}

// ---------------------------------------------------------------------------
// Module entry point: db.open(driver, dsn, opts?)
// ---------------------------------------------------------------------------

var stdDbMethods map[string]StdModuleFunc

func init() {
	stdDbMethods = map[string]StdModuleFunc{
		"open":    stdDbOpen,
		"drivers": stdDbDrivers,
	}
}

func (vm *VM) callStdDb(method string, args []TinyValue) {
	fn, ok := stdDbMethods[method]
	if !ok {
		vm.runtimeError(ErrorName, "unknown db function: %s", method)
		return
	}
	fn(vm, args)
}

func stdDbDrivers(vm *VM, args []TinyValue) {
	dontExpectArgs(vm, "db.drivers", args)
	names := []TinyValue{
		NewNative("sqlite"), NewNative("postgres"), NewNative("mysql"),
	}
	vm.push(NewNative(&ArrayValue{Elements: names}))
}

func stdDbOpen(vm *VM, args []TinyValue) {
	expectArgsRange(vm, "db.open", args, 2, 3)
	driverName := argString(vm, "db.open", args, 0)
	dsn := argString(vm, "db.open", args, 1)

	driver, ok := dbDrivers[strings.ToLower(driverName)]
	if !ok {
		vm.runtimeError(ErrorRuntime, "db.open: unknown driver %q (have: sqlite, postgres, mysql)", driverName)
		return
	}

	connStr := buildDSN(driver, driverName, dsn)

	conn, err := sql.Open(driver.sqlName, connStr)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "db.open: %v", err)
		return
	}

	inMemorySqlite := driver.sqlName == "sqlite" && (dsn == ":memory:" || strings.HasPrefix(dsn, "file::memory:"))
	if inMemorySqlite {
		conn.SetMaxOpenConns(1)
	}

	// Optional { maxOpenConns, maxIdleConns } configuration object.
	if len(args) == 3 {
		opts := argObject(vm, "db.open", args, 2)
		if v, ok := opts["maxOpenConns"]; ok {
			conn.SetMaxOpenConns(asInt(v))
		}
		if v, ok := opts["maxIdleConns"]; ok {
			conn.SetMaxIdleConns(asInt(v))
		}
	}

	if err := conn.Ping(); err != nil {
		conn.Close()
		vm.runtimeError(ErrorRuntime, "db.open: cannot connect: %v", err)
		return
	}

	vm.push(NewNative(&NativeDBValue{db: conn, driver: driver, dsn: connStr}))
}

func buildDSN(driver dbDriver, driverName, dsn string) string {
	if driver.sqlName != "sqlite" {
		return dsn
	}
	if dsn == ":memory:" || strings.HasPrefix(dsn, "file:") {
		return dsn
	}
	// WAL + busy timeout are sane defaults for a file-backed SQLite database.
	return "file:" + dsn + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func asInt(v TinyValue) int {
	if v.IsInt {
		return v.AsInt
	}
	if f, ok := v.Value.(float64); ok {
		return int(f)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Connection methods
// ---------------------------------------------------------------------------

var dbMethods map[string]NativeModuleFunc[*NativeDBValue]

func init() {
	dbMethods = map[string]NativeModuleFunc[*NativeDBValue]{
		"exec":    dbExec,
		"run":     dbRun,
		"query":   dbQuery,
		"get":     dbGet,
		"prepare": dbPrepare,
		"begin":   dbBegin,
		"close":   dbClose,
	}
}

func (vm *VM) callDBMethod(db *NativeDBValue, method string, args []TinyValue) {
	fn, ok := dbMethods[method]
	if !ok {
		vm.runtimeError(ErrorName, "unknown database method: %s", method)
		return
	}
	if db.closed || db.db == nil {
		if method != "close" {
			vm.runtimeError(ErrorRuntime, "db.%s: database is closed", method)
			return
		}
	}
	fn(vm, db, args)
}

func dbExec(vm *VM, db *NativeDBValue, args []TinyValue) {
	expectArgsRange(vm, "db.exec", args, 1, 2)
	vm.execStatement(db.db, db.driver, "db.exec", args, false)
}

func dbRun(vm *VM, db *NativeDBValue, args []TinyValue) {
	expectArgsRange(vm, "db.run", args, 1, 2)
	vm.execStatement(db.db, db.driver, "db.run", args, true)
}

func dbQuery(vm *VM, db *NativeDBValue, args []TinyValue) {
	expectArgsRange(vm, "db.query", args, 1, 2)
	vm.queryStatement(db.db, db.driver, "db.query", args, false)
}

func dbGet(vm *VM, db *NativeDBValue, args []TinyValue) {
	expectArgsRange(vm, "db.get", args, 1, 2)
	vm.queryStatement(db.db, db.driver, "db.get", args, true)
}

func dbPrepare(vm *VM, db *NativeDBValue, args []TinyValue) {
	expectArgs(vm, "db.prepare", args, 1)
	query := argString(vm, "db.prepare", args, 0)

	// Prepared statements bind positionally, so rebind ? -> $N up front for pg.
	rewritten, _, err := rewritePositional(db.driver, query, nil)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "db.prepare: %v", err)
		return
	}
	stmt, err := db.db.Prepare(rewritten)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "db.prepare: %v", err)
		return
	}
	vm.push(NewNative(&NativeStmtValue{stmt: stmt, driver: db.driver}))
}

func dbBegin(vm *VM, db *NativeDBValue, args []TinyValue) {
	dontExpectArgs(vm, "db.begin", args)
	tx, err := db.db.Begin()
	if err != nil {
		vm.runtimeError(ErrorRuntime, "db.begin: %v", err)
		return
	}
	vm.push(NewNative(&NativeTxValue{tx: tx, driver: db.driver}))
}

func dbClose(vm *VM, db *NativeDBValue, args []TinyValue) {
	dontExpectArgs(vm, "db.close", args)
	if db.closed || db.db == nil {
		vm.push(NewNull())
		return
	}
	if err := db.db.Close(); err != nil {
		vm.runtimeError(ErrorRuntime, "db.close: %v", err)
		return
	}
	db.closed = true
	vm.push(NewNull())
}

// ---------------------------------------------------------------------------
// Transaction methods
// ---------------------------------------------------------------------------

var txMethods map[string]NativeModuleFunc[*NativeTxValue]

func init() {
	txMethods = map[string]NativeModuleFunc[*NativeTxValue]{
		"exec":     txExec,
		"run":      txRun,
		"query":    txQuery,
		"get":      txGet,
		"commit":   txCommit,
		"rollback": txRollback,
	}
}

func (vm *VM) callTxMethod(tx *NativeTxValue, method string, args []TinyValue) {
	fn, ok := txMethods[method]
	if !ok {
		vm.runtimeError(ErrorName, "unknown transaction method: %s", method)
		return
	}
	if tx.done && method != "rollback" && method != "commit" {
		vm.runtimeError(ErrorRuntime, "tx.%s: transaction already finished", method)
		return
	}
	fn(vm, tx, args)
}

func txExec(vm *VM, tx *NativeTxValue, args []TinyValue) {
	expectArgsRange(vm, "tx.exec", args, 1, 2)
	vm.execStatement(tx.tx, tx.driver, "tx.exec", args, false)
}

func txRun(vm *VM, tx *NativeTxValue, args []TinyValue) {
	expectArgsRange(vm, "tx.run", args, 1, 2)
	vm.execStatement(tx.tx, tx.driver, "tx.run", args, true)
}

func txQuery(vm *VM, tx *NativeTxValue, args []TinyValue) {
	expectArgsRange(vm, "tx.query", args, 1, 2)
	vm.queryStatement(tx.tx, tx.driver, "tx.query", args, false)
}

func txGet(vm *VM, tx *NativeTxValue, args []TinyValue) {
	expectArgsRange(vm, "tx.get", args, 1, 2)
	vm.queryStatement(tx.tx, tx.driver, "tx.get", args, true)
}

func txCommit(vm *VM, tx *NativeTxValue, args []TinyValue) {
	dontExpectArgs(vm, "tx.commit", args)
	if tx.done {
		vm.runtimeError(ErrorRuntime, "tx.commit: transaction already finished")
		return
	}
	if err := tx.tx.Commit(); err != nil {
		vm.runtimeError(ErrorRuntime, "tx.commit: %v", err)
		return
	}
	tx.done = true
	vm.push(NewNull())
}

func txRollback(vm *VM, tx *NativeTxValue, args []TinyValue) {
	dontExpectArgs(vm, "tx.rollback", args)
	if tx.done {
		vm.push(NewNull())
		return
	}
	if err := tx.tx.Rollback(); err != nil {
		vm.runtimeError(ErrorRuntime, "tx.rollback: %v", err)
		return
	}
	tx.done = true
	vm.push(NewNull())
}

// ---------------------------------------------------------------------------
// Prepared statement methods
// ---------------------------------------------------------------------------

var stmtMethods map[string]NativeModuleFunc[*NativeStmtValue]

func init() {
	stmtMethods = map[string]NativeModuleFunc[*NativeStmtValue]{
		"run":   stmtRun,
		"query": stmtQuery,
		"get":   stmtGet,
		"close": stmtClose,
	}
}

func (vm *VM) callStmtMethod(stmt *NativeStmtValue, method string, args []TinyValue) {
	fn, ok := stmtMethods[method]
	if !ok {
		vm.runtimeError(ErrorName, "unknown statement method: %s", method)
		return
	}
	if stmt.closed && method != "close" {
		vm.runtimeError(ErrorRuntime, "stmt.%s: statement is closed", method)
		return
	}
	fn(vm, stmt, args)
}

func stmtRun(vm *VM, stmt *NativeStmtValue, args []TinyValue) {
	expectArgsRange(vm, "stmt.run", args, 0, 1)
	params := vm.bindPositionalArgs("stmt.run", args, 0)
	res, err := stmt.stmt.Exec(params...)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "stmt.run: %v", err)
		return
	}
	vm.push(execResult(res))
}

func stmtQuery(vm *VM, stmt *NativeStmtValue, args []TinyValue) {
	expectArgsRange(vm, "stmt.query", args, 0, 1)
	params := vm.bindPositionalArgs("stmt.query", args, 0)
	rows, err := stmt.stmt.Query(params...)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "stmt.query: %v", err)
		return
	}
	vm.scanRows(rows, "stmt.query", false)
}

func stmtGet(vm *VM, stmt *NativeStmtValue, args []TinyValue) {
	expectArgsRange(vm, "stmt.get", args, 0, 1)
	params := vm.bindPositionalArgs("stmt.get", args, 0)
	rows, err := stmt.stmt.Query(params...)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "stmt.get: %v", err)
		return
	}
	vm.scanRows(rows, "stmt.get", true)
}

func stmtClose(vm *VM, stmt *NativeStmtValue, args []TinyValue) {
	dontExpectArgs(vm, "stmt.close", args)
	if stmt.closed {
		vm.push(NewNull())
		return
	}
	if err := stmt.stmt.Close(); err != nil {
		vm.runtimeError(ErrorRuntime, "stmt.close: %v", err)
		return
	}
	stmt.closed = true
	vm.push(NewNull())
}

// ---------------------------------------------------------------------------
// Shared exec / query helpers
// ---------------------------------------------------------------------------

func (vm *VM) execStatement(qe queryExec, driver dbDriver, where string, args []TinyValue, withResult bool) {
	query := argString(vm, where, args, 0)
	rewritten, params, ok := vm.bindParams(driver, where, query, args, 1)
	if !ok {
		return
	}
	res, err := qe.Exec(rewritten, params...)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
		return
	}
	if withResult {
		vm.push(execResult(res))
	} else {
		vm.push(NewNull())
	}
}

func (vm *VM) queryStatement(qe queryExec, driver dbDriver, where string, args []TinyValue, single bool) {
	query := argString(vm, where, args, 0)
	rewritten, params, ok := vm.bindParams(driver, where, query, args, 1)
	if !ok {
		return
	}
	rows, err := qe.Query(rewritten, params...)
	if err != nil {
		vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
		return
	}
	vm.scanRows(rows, where, single)
}

func execResult(res sql.Result) TinyValue {
	changes, _ := res.RowsAffected()
	lastID, _ := res.LastInsertId() // unsupported on postgres -> 0
	return NewNative(ObjectValue{
		"changes":         NewInt(int(changes)),
		"lastInsertRowid": NewInt(int(lastID)),
	})
}

func (vm *VM) scanRows(rows *sql.Rows, where string, single bool) {
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
		return
	}
	colTypes, _ := rows.ColumnTypes()

	var out []TinyValue
	for rows.Next() {
		cells := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
			return
		}
		obj := make(ObjectValue, len(cols))
		for i, name := range cols {
			var typeName string
			if colTypes != nil && i < len(colTypes) {
				typeName = colTypes[i].DatabaseTypeName()
			}
			obj[name] = sqlToTiny(cells[i], typeName)
		}
		row := NewNative(obj)
		if single {
			vm.push(row)
			return
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
		return
	}
	if single {
		vm.push(NewNull())
		return
	}
	vm.push(NewNative(&ArrayValue{Elements: out}))
}

// ---------------------------------------------------------------------------
// Value conversion
// ---------------------------------------------------------------------------

func tinyToSQL(v TinyValue) any {
	if v.IsInt {
		return int64(v.AsInt)
	}
	switch val := v.Value.(type) {
	case float64:
		return val
	case string:
		return val
	case bool:
		return val
	case *BufferValue:
		return val.Bytes
	case NullValue, nil:
		return nil
	default:
		return fmt.Sprintf("%v", val)
	}
}

func sqlToTiny(v any, dbType string) TinyValue {
	switch val := v.(type) {
	case nil:
		return NewNull()
	case int64:
		return NewInt(int(val))
	case int32:
		return NewInt(int(val))
	case int16:
		return NewInt(int(val))
	case int:
		return NewInt(val)
	case float64:
		return NewNative(val)
	case float32:
		return NewNative(float64(val))
	case bool:
		return NewNative(val)
	case string:
		return NewNative(val)
	case []byte:
		// Drivers differ: MySQL returns TEXT/DECIMAL as []byte, SQLite returns
		// BLOB as []byte. Use the declared column type to pick a sensible Tiny
		// representation, defaulting text-like data to a string.
		if isBinaryColumn(dbType) {
			return NewNative(&BufferValue{Bytes: append([]byte(nil), val...)})
		}
		return NewNative(string(val))
	case time.Time:
		return NewNative(val.Format(time.RFC3339Nano))
	default:
		return NewNative(fmt.Sprintf("%v", val))
	}
}

func isBinaryColumn(dbType string) bool {
	t := strings.ToUpper(dbType)
	return strings.Contains(t, "BLOB") || strings.Contains(t, "BINARY") || strings.Contains(t, "BYTEA")
}

// ---------------------------------------------------------------------------
// Parameter binding: positional arrays and named objects, with rebinding.
// ---------------------------------------------------------------------------

// bindParams inspects the optional argument at args[index]: an array binds
// positionally (?), an object binds by name (:key). It returns the SQL with
// driver-appropriate placeholders and the ordered argument list.
func (vm *VM) bindParams(driver dbDriver, where, query string, args []TinyValue, index int) (string, []any, bool) {
	if index >= len(args) {
		return query, nil, true
	}
	if _, isNull := args[index].Value.(NullValue); isNull {
		return query, nil, true
	}

	switch val := args[index].Value.(type) {
	case *ArrayValue:
		rewritten, params, err := rewritePositional(driver, query, val.Elements)
		if err != nil {
			vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
			return "", nil, false
		}
		return rewritten, params, true
	case ObjectValue:
		rewritten, params, err := rewriteNamed(driver, query, val)
		if err != nil {
			vm.runtimeError(ErrorRuntime, "%s: %v", where, err)
			return "", nil, false
		}
		return rewritten, params, true
	default:
		vm.runtimeError(ErrorType, "%s: parameters must be an array or object, got %s", where, TypeName(args[index]))
		return "", nil, false
	}
}

// bindPositionalArgs converts an optional positional array into driver args.
// Used by prepared statements, whose SQL was already rebound at prepare time.
func (vm *VM) bindPositionalArgs(where string, args []TinyValue, index int) []any {
	if index >= len(args) {
		return nil
	}
	if _, isNull := args[index].Value.(NullValue); isNull {
		return nil
	}
	arr := argArray(vm, where, args, index)
	params := make([]any, len(arr.Elements))
	for i, el := range arr.Elements {
		params[i] = tinyToSQL(el)
	}
	return params
}

// rewritePositional converts ? placeholders to $N for postgres and collects
// the ordered argument list. Quoted strings are skipped so '?' inside a
// literal is left untouched.
func rewritePositional(driver dbDriver, query string, elements []TinyValue) (string, []any, error) {
	params := make([]any, 0, len(elements))
	for _, el := range elements {
		params = append(params, tinyToSQL(el))
	}

	if !driver.postgres {
		return query, params, nil
	}

	var b strings.Builder
	n := 0
	inSingle, inDouble := false, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case inSingle:
			b.WriteByte(c)
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			b.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
			b.WriteByte(c)
		case c == '"':
			inDouble = true
			b.WriteByte(c)
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), params, nil
}

// rewriteNamed replaces :name tokens with driver placeholders, building the
// argument list in the order the names appear. Quoted strings and the ::cast
// operator are skipped.
func rewriteNamed(driver dbDriver, query string, obj ObjectValue) (string, []any, error) {
	lookup := make(map[string]TinyValue, len(obj))
	for k, v := range obj {
		lookup[fmt.Sprintf("%v", k)] = v
	}

	var b strings.Builder
	params := make([]any, 0, len(obj))
	n := 0
	inSingle, inDouble := false, false

	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case inSingle:
			b.WriteByte(c)
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			b.WriteByte(c)
			if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
			b.WriteByte(c)
		case c == '"':
			inDouble = true
			b.WriteByte(c)
		case c == ':' && i+1 < len(query) && query[i+1] == ':':
			// Postgres ::type cast — copy both colons verbatim.
			b.WriteString("::")
			i++
		case c == ':' && i+1 < len(query) && isNameStart(query[i+1]):
			j := i + 1
			for j < len(query) && isNameChar(query[j]) {
				j++
			}
			name := query[i+1 : j]
			val, ok := lookup[name]
			if !ok {
				return "", nil, fmt.Errorf("missing named parameter :%s", name)
			}
			params = append(params, tinyToSQL(val))
			n++
			if driver.postgres {
				b.WriteByte('$')
				b.WriteString(strconv.Itoa(n))
			} else {
				b.WriteByte('?')
			}
			i = j - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), params, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}
