package sqinn

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
)

// function codes, see sqinn/src/handler.h

const (
	fcSqinnVersion  byte = 1
	fcIoVersion     byte = 2
	fcSqliteVersion byte = 3
	fcOpen          byte = 10
	fcPrepare       byte = 11
	fcBind          byte = 12
	fcStep          byte = 13
	fcReset         byte = 14
	fcChanges       byte = 15
	fcColumn        byte = 16
	fcFinalize      byte = 17
	fcClose         byte = 18
	fcExec          byte = 51
	fcQuery         byte = 52
)

// Options for launching a Sqinn instance.
type Options struct {

	// Path to Sqinn executable. Can be an absolute or relative path.
	// Empty is the same as "sqinn". Default is empty.
	SqinnPath string

	// Logger logs the debug and error messages that the sinn subprocess will output
	// on its stderr. Default is nil, which does not log anything.
	Logger Logger
}

// Sqinn is a running sqinn instance.
type Sqinn struct {
	mx   sync.Mutex
	cmd  *exec.Cmd
	sin  io.WriteCloser
	sout io.ReadCloser
	serr io.ReadCloser
}

/*
New launches a new Sqinn instance. The options argument specifies
the path to the sqinn executable. Moreover, it specifies how Sqinn's
stderr log outputs should be logged.
*/
func New(options Options) (*Sqinn, error) {
	sqinnPath := options.SqinnPath
	if sqinnPath == "" {
		sqinnPath = "sqinn"
	}
	cmd := exec.Command(sqinnPath)
	sin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	sout, err := cmd.StdoutPipe()
	if err != nil {
		sin.Close()
		return nil, err
	}
	serr, err := cmd.StderrPipe()
	if err != nil {
		sout.Close()
		sin.Close()
		return nil, err
	}
	err = cmd.Start()
	if err != nil {
		serr.Close()
		sout.Close()
		sin.Close()
		return nil, err
	}
	sq := &Sqinn{sync.Mutex{}, cmd, sin, sout, serr}
	logger := options.Logger
	if logger == nil {
		logger = NoLogger{}
	}
	go sq.run(logger)
	return sq, nil
}

func (sq *Sqinn) run(logger Logger) {
	sc := bufio.NewScanner(sq.serr)
	for sc.Scan() {
		text := sc.Text()
		logger.Log(fmt.Sprintf("[sqinn] %s", text))
	}
	err := sc.Err()
	if err != nil {
		logger.Log(fmt.Sprintf("[sqinn] stderr: %s", err))
	}
}

// SqinnVersion returns the version of the Sqinn executable.
func (sq *Sqinn) SqinnVersion(filename string) (string, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcSqinnVersion}
	// resp
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return "", err
	}
	var version string
	version, resp = decodeString(resp)
	return version, nil
}

// IoVersion returns the protocol version for this Sqinn instance.
func (sq *Sqinn) IoVersion() (byte, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcIoVersion}
	// resp
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return 0, err
	}
	var version byte
	version, resp = decodeByte(resp)
	return version, nil
}

// SqliteVersion returns the SQLite library version Sqinn was built with.
func (sq *Sqinn) SqliteVersion(filename string) (string, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcSqliteVersion}
	// resp
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return "", err
	}
	var version string
	version, resp = decodeString(resp)
	return version, nil
}

// Open opens a database.
// The filename can be ":memory:" or any filesystem path, e.g. "/tmp/test.db".
// Sqinn keeps the database open until Close is called. After Close has been
// called, this Sqinn instance can be terminated with Terminate, or Open can be
// called again, either on the same database or on a different one. For every
// Open there should be a Close call.
//
// For further details, see https://www.sqlite.org/c3ref/open.html.
func (sq *Sqinn) Open(filename string) error {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := make([]byte, 0, 10+len(filename))
	req = append(req, fcOpen)
	req = append(req, encodeString(filename)...)
	// resp
	_, err := sq.writeAndRead(req)
	if err != nil {
		return err
	}
	return nil
}

// Prepare prepares a statement, using the provided sql string.
// To avoid memory leaks, each prepared statement must be finalized
// after use. Sqinn allows only one prepared statement at at time,
// preparing a statement while another statement is still active
// (not yet finalized) will result in a error.
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/prepare.html.
func (sq *Sqinn) Prepare(sql string) error {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := make([]byte, 0, 10+len(sql))
	req = append(req, fcPrepare)
	req = append(req, encodeString(sql)...)
	// resp
	_, err := sq.writeAndRead(req)
	if err != nil {
		return err
	}
	return nil
}

func (sq *Sqinn) bindValue(req []byte, value interface{}) ([]byte, error) {
	switch v := value.(type) {
	case nil:
		req = append(req, ValNull)
	case int:
		req = append(req, ValInt)
		req = append(req, encodeInt32(v)...)
	case int64:
		req = append(req, ValInt64)
		req = append(req, encodeInt64(v)...)
	case float64:
		req = append(req, ValDouble)
		req = append(req, encodeDouble(float64(v))...)
	case string:
		req = append(req, ValText)
		req = append(req, encodeString(v)...)
	case []byte:
		req = append(req, ValBlob)
		req = append(req, encodeBlob(v)...)
	default:
		return nil, fmt.Errorf("cannot bind type %T", v)
	}
	return req, nil
}

func (sq *Sqinn) bindValues(req []byte, values []interface{}) ([]byte, error) {
	var err error
	for _, value := range values {
		req, err = sq.bindValue(req, value)
		if err != nil {
			return nil, err
		}
	}
	return req, nil
}

// Bind binds the iparam'th parameter with the specified value.
// The value can be an int, int64, float64, string, []byte or nil.
// Not that iparam starts at 1 (not 0):
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/bind_blob.html.
func (sq *Sqinn) Bind(iparam int, value interface{}) error {
	if iparam < 1 {
		return fmt.Errorf("Bind: iparam must be >= 1 but was %d", iparam)
	}
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := make([]byte, 0, 6)
	req = append(req, fcBind)
	req = append(req, encodeInt32(iparam)...)
	var err error
	req, err = sq.bindValue(req, value)
	if err != nil {
		return err
	}
	// resp
	_, err = sq.writeAndRead(req)
	if err != nil {
		return err
	}
	return nil
}

// Step advances the current statement to the next row or to completion.
// It returns true if there are more rows available, false if not.
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/step.html.
func (sq *Sqinn) Step() (bool, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcStep}
	// resp
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return false, err
	}
	more, _ := decodeBool(resp)
	return more, nil
}

// Reset resets the current statement to its initial state.
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/reset.html.
func (sq *Sqinn) Reset() error {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcReset}
	// resp
	_, err := sq.writeAndRead(req)
	if err != nil {
		return err
	}
	return nil
}

// Changes counts the number of rows modified by the last SQL operation.
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/changes.html.
func (sq *Sqinn) Changes() (int, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcChanges}
	// resp
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return 0, err
	}
	var changes int
	changes, resp = decodeInt32(resp)
	return changes, nil
}

func (sq *Sqinn) decodeAnyValue(resp []byte, colType byte) (AnyValue, []byte, error) {
	var any AnyValue
	var set bool
	set, resp = decodeBool(resp)
	if set {
		switch colType {
		case ValNull:
			// user wants NULL, will get NULL
		case ValInt:
			any.Int.Set = true
			any.Int.Value, resp = decodeInt32(resp)
		case ValInt64:
			any.Int64.Set = true
			any.Int64.Value, resp = decodeInt64(resp)
		case ValDouble:
			any.Double.Set = true
			any.Double.Value, resp = decodeDouble(resp)
		case ValText:
			any.String.Set = true
			any.String.Value, resp = decodeString(resp)
		case ValBlob:
			any.Blob.Set = true
			any.Blob.Value, resp = decodeBlob(resp)
		default:
			return any, resp, fmt.Errorf("invalid col type %d", colType)
		}
	}
	return any, resp, nil
}

// Column retrieves the value of the icol'th column.
// The colType specifies the expected type of the column value.
// Note that icol starts at 0 (not 1).
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/column_blob.html.
func (sq *Sqinn) Column(icol int, colType byte) (AnyValue, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := make([]byte, 0, 6)
	req = append(req, fcColumn)
	req = append(req, encodeInt32(icol)...)
	req = append(req, colType)
	// resp
	var any AnyValue
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return any, err
	}
	set, resp := decodeBool(resp)
	if !set {
		return any, nil
	}
	any, _, err = sq.decodeAnyValue(resp, colType)
	return any, err
}

// Finalize finalizes a statement that has been prepared with Prepare.
// To avoid memory leaks, each statement has to be finalized.
// Moreover, since Sqinn allows only one statement at a time,
// each statement must be finalized before a new statement can be prepared.
//
// This is a low-level function. Most users will use Exec/Query instead.
//
// For further details, see https://www.sqlite.org/c3ref/finalize.html.
func (sq *Sqinn) Finalize() error {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcFinalize}
	// resp
	_, err := sq.writeAndRead(req)
	return err
}

// Close closes the database connection that has been opened with Open.
// After Close has been called, this Sqinn instance can be terminated, or
// another database can be opened with Open.
//
// For further details, see https://www.sqlite.org/c3ref/close.html.
func (sq *Sqinn) Close() error {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// req
	req := []byte{fcClose}
	// resp
	_, err := sq.writeAndRead(req)
	return err
}

// ExecOne executes a SQL statement and returns the number of modified rows.
// It is used primarily for short, simple statements that have no parameters
// and do not query rows. A good use case is for beginning and committing
// a transaction:
//
//     _, err = sq.ExecOne("BEGIN TRANSACTION");
//     // do stuff in tx
//     _, err = sq.ExecOne("COMMIT");
//
// Another use case is for DDL statements:
//
//     _, err = sq.ExecOne("DROP TABLE users");
//     _, err = sq.ExecOne("CREATE TABLE foo (name VARCHAR)");
//
// ExecOne(sql) has the same effect as Exec(sql, 1, 0, nil).
//
// If a error occurs, ExecOne will return (0, err).
func (sq *Sqinn) ExecOne(sql string) (int, error) {
	changes, err := sq.Exec(sql, 1, 0, nil)
	if err != nil {
		return 0, err
	}
	return changes[0], nil
}

// MustExecOne is like ExecOne except it panics on error.
func (sq *Sqinn) MustExecOne(sql string) int {
	mod, err := sq.ExecOne(sql)
	if err != nil {
		panic(err)
	}
	return mod
}

// Exec executes a SQL statement multiple times and returns the
// number of modified rows for each iteration. It supports bind parmeters.
// Exec is used to execute SQL statements that do not return results (see
// Query for those).
//
// The niterations tells Exec how often to run the sql. It must be >= 0 and
// should be >= 1. If niterations is zero, the statement is not run at all,
// and the method call is a waste of CPU cycles.
//
// Binding sql parameters is possible with the nparams and values arguments.
// The nparams argument tells Exec how many parameters to bind per iteration.
// nparams must be >= 0.
//
// The values argument holds the parameter values. Parameter values can be
// of the following type: int, int64, float64, string, []byte or nil.
// The length of values must always be niterations * nparams.
//
// Internally, Exec preapres a statement, binds nparams parameters, steps
// the statement, resets the statement, binds the next nparams parameters,
// and so on, until niterations is reached.
//
// Exec returns, for each iteration, the count of modified rows. The
// resulting int slice will always be of length niterations.
//
// If an error occurs, Exec will return (nil, err).
func (sq *Sqinn) Exec(sql string, niterations, nparams int, values []interface{}) ([]int, error) {
	if niterations < 0 {
		return nil, fmt.Errorf("Exec '%s' niterations must be >= 0 but was %d", sql, niterations)
	}
	if len(values) != niterations*nparams {
		return nil, fmt.Errorf("Exec '%s' expected %d values but have %d", sql, niterations*nparams, len(values))
	}
	sq.mx.Lock()
	defer sq.mx.Unlock()
	req := make([]byte, 0, 10+len(sql))
	req = append(req, fcExec)
	req = append(req, encodeString(sql)...)
	req = append(req, encodeInt32(niterations)...)
	req = append(req, encodeInt32(nparams)...)
	var err error
	req, err = sq.bindValues(req, values)
	if err != nil {
		return nil, err
	}
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return nil, err
	}
	changes := make([]int, niterations)
	for i := 0; i < niterations; i++ {
		changes[i], resp = decodeInt32(resp)
	}
	return changes, nil
}

// MustExec is like Exec except it panics on error.
func (sq *Sqinn) MustExec(sql string, niterations, nparams int, values []interface{}) []int {
	mods, err := sq.Exec(sql, niterations, nparams, values)
	if err != nil {
		panic(err)
	}
	return mods
}

// Query executes a SQL statement and returns all rows. It supports bind parmeters.
// Query is used mostly for SELECT statements or PRAGMA statements that return rows.
//
// The values argument holds a list of bind parameters. Values must be of type
// int, int64, float64, string or []byte.
//
// The colTypes argument holds a list of column types that the query yields.
//
// Query returns all resulting rows at once. There is no way
// to interrupt a Query while it is running. If a Query yields more data
// than can fit into memory, the behavior is undefined, most likely an out-of-memory
// condition will crash your program. It is up to the caller to make sure
// that all queried data fits into memory. The sql 'LIMIT' operator may be helpful.
//
// If an error occurs, Query will return (nil, err).
func (sq *Sqinn) Query(sql string, values []interface{}, colTypes []byte) ([]Row, error) {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	req := make([]byte, 0, 10+len(sql))
	req = append(req, fcQuery)
	req = append(req, encodeString(sql)...)
	nparams := len(values)
	req = append(req, encodeInt32(nparams)...)
	var err error
	req, err = sq.bindValues(req, values)
	ncols := len(colTypes)
	req = append(req, encodeInt32(ncols)...)
	req = append(req, colTypes...)
	resp, err := sq.writeAndRead(req)
	if err != nil {
		return nil, err
	}
	var nrows int
	nrows, resp = decodeInt32(resp)
	rows := make([]Row, 0, nrows)
	for i := 0; i < nrows; i++ {
		var row Row
		row.Values = make([]AnyValue, 0, ncols)
		for icol := 0; icol < ncols; icol++ {
			var any AnyValue
			any, resp, err = sq.decodeAnyValue(resp, colTypes[icol])
			if err != nil {
				return nil, err
			}
			row.Values = append(row.Values, any)
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// MustQuery is like Query except it panics on error.
func (sq *Sqinn) MustQuery(sql string, values []interface{}, colTypes []byte) []Row {
	rows, err := sq.Query(sql, values, colTypes)
	if err != nil {
		panic(err)
	}
	return rows
}

func (sq *Sqinn) writeAndRead(req []byte) ([]byte, error) {
	traceReq := false
	traceResp := false
	// write req
	sz := len(req)
	buf := make([]byte, 0, 4+len(req))
	buf = append(buf, encodeInt32(sz)...)
	buf = append(buf, req...)
	if traceReq {
		log.Printf("write %d bytes sz+req: %v", len(buf), buf)
	}
	_, err := sq.sin.Write(buf)
	if err != nil {
		return nil, err
	}
	// read resp
	if traceResp {
		// time.Sleep(100 * time.Millisecond)
		log.Printf("waiting for 4 bytes resp sz")
	}
	buf = make([]byte, 4)
	_, err = io.ReadFull(sq.sout, buf)
	if err != nil {
		return nil, fmt.Errorf("while reading from sqinn: %w", err)
	}
	if traceResp {
		log.Printf("received %d bytes resp length: %v", len(buf), buf)
	}
	sz, _ = decodeInt32(buf)
	if traceResp {
		log.Printf("resp length will be %d bytes", sz)
	}
	if sz <= 0 {
		return nil, fmt.Errorf("invalid response size %d", sz)
	}
	buf = make([]byte, sz)
	if traceResp {
		log.Printf("waiting for %d resp data", sz)
	}
	_, err = io.ReadFull(sq.sout, buf)
	if err != nil {
		return nil, fmt.Errorf("while reading from sqinn: %w", err)
	}
	if traceResp {
		log.Printf("received %d bytes resp data: %v", len(buf), buf)
		// time.Sleep(100 * time.Millisecond)
	}
	var ok bool
	ok, buf = decodeBool(buf)
	if !ok {
		msg, _ := decodeString(buf)
		return nil, fmt.Errorf("sqinn: %s", msg)
	}
	return buf, nil
}

// Terminate terminates a running Sqinn instance.
// Each Sqinn instance launched with New should be terminated
// with Terminate. After Terminate has been called, this Sqinn
// instance must not be used any more.
func (sq *Sqinn) Terminate() error {
	sq.mx.Lock()
	defer sq.mx.Unlock()
	// a request of length zero makes sqinn terminate
	_, err := sq.sin.Write(encodeInt32(0))
	if err != nil {
		return err
	}
	err = sq.cmd.Wait()
	if err != nil {
		return err
	}
	sq.serr.Close()
	sq.sout.Close()
	sq.sin.Close()
	return nil
}