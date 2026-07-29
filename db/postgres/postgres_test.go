package postgres

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"
)

type connectorStub struct {
	conn driver.Conn
	err  error
}

func (c connectorStub) Connect(context.Context) (driver.Conn, error) {
	return c.conn, c.err
}

func (connectorStub) Driver() driver.Driver {
	return driverStub{}
}

type driverStub struct{}

func (driverStub) Open(string) (driver.Conn, error) {
	return nil, errors.New("not implemented")
}

type connStub struct {
	closed bool
}

func (*connStub) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("not implemented")
}

func (c *connStub) Close() error {
	c.closed = true
	return nil
}

func (*connStub) Begin() (driver.Tx, error) {
	return nil, errors.New("not implemented")
}

type execContextConnStub struct {
	connStub
	ctx   context.Context
	query string
	args  []driver.NamedValue
	err   error
}

func (c *execContextConnStub) ExecContext(
	ctx context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Result, error) {
	c.ctx = ctx
	c.query = query
	c.args = args
	return driver.RowsAffected(0), c.err
}

func TestSessionConnectorUsesExecContext(t *testing.T) {
	t.Parallel()

	conn := &execContextConnStub{}
	schema := "tenant_a"
	connector := &sessionConnector{
		Connector: connectorStub{conn: conn},
		session: SessionConfig{
			TimeZone:         "UTC",
			ApplicationName:  "maniflex-test",
			SchemaName:       &schema,
			StatementTimeout: 2 * time.Second,
		},
	}
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "request")

	got, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got != conn {
		t.Fatalf("connection = %T %p, want original %T %p", got, got, conn, conn)
	}
	if conn.ctx != ctx {
		t.Error("ExecContext did not receive the Connect context")
	}
	if len(conn.args) != 0 {
		t.Errorf("ExecContext args = %#v, want none", conn.args)
	}
	for _, want := range []string{
		"SET TIME ZONE 'UTC'",
		"SET application_name = 'maniflex-test'",
		"SET statement_timeout = 2000",
		"SET search_path TO tenant_a",
	} {
		if !strings.Contains(conn.query, want) {
			t.Errorf("session SQL %q does not contain %q", conn.query, want)
		}
	}
	if conn.closed {
		t.Error("successful session initialization closed the connection")
	}
}

func TestSessionConnectorClosesConnectionOnExecContextError(t *testing.T) {
	t.Parallel()

	execErr := errors.New("session rejected")
	conn := &execContextConnStub{err: execErr}
	connector := &sessionConnector{
		Connector: connectorStub{conn: conn},
		session:   SessionConfig{TimeZone: "UTC", ApplicationName: "test"},
	}

	got, err := connector.Connect(context.Background())
	if got != nil {
		t.Fatalf("connection = %T, want nil", got)
	}
	if !errors.Is(err, execErr) {
		t.Fatalf("error = %v, want wrapped %v", err, execErr)
	}
	if !conn.closed {
		t.Error("failed session initialization did not close the connection")
	}
}

func TestSessionConnectorRejectsLegacyExecOnlyConnection(t *testing.T) {
	t.Parallel()

	conn := &connStub{}
	connector := &sessionConnector{
		Connector: connectorStub{conn: conn},
		session:   SessionConfig{TimeZone: "UTC", ApplicationName: "test"},
	}

	got, err := connector.Connect(context.Background())
	if got != nil {
		t.Fatalf("connection = %T, want nil", got)
	}
	if err == nil || !strings.Contains(err.Error(), "driver.ExecerContext") {
		t.Fatalf("error = %v, want driver.ExecerContext requirement", err)
	}
	if !conn.closed {
		t.Error("unsupported connection was not closed")
	}
}
