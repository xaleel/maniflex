package sqlcore

// Two replicas booting at once both find a foreign key absent and both issue
// ALTER TABLE ADD CONSTRAINT. Postgres has no IF NOT EXISTS for ADD CONSTRAINT,
// so the loser is told the constraint already exists — and until that is
// tolerated, the losing replica's boot fails on a schema that is already
// correct. addColumn has handled the same race for columns all along.

import (
	"errors"
	"testing"
)

func TestIsDuplicateConstraintError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "postgres duplicate constraint (SQLSTATE 42710)",
			err: errors.New(
				`pq: constraint "fk_orders_customer_id" for relation "orders" already exists`),
			want: true,
		},
		{
			name: "pgx wording",
			err: errors.New(
				`ERROR: constraint "fk_items_order_id" for relation "items" already exists (SQLSTATE 42710)`),
			want: true,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
		{
			// The constraint could not be added because the data violates it.
			// That is a real migration failure and must still fail the boot.
			name: "existing rows violate the constraint",
			err: errors.New(
				`pq: insert or update on table "orders" violates foreign key constraint "fk_orders_customer_id"`),
			want: false,
		},
		{
			name: "unrelated failure",
			err:  errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
			want: false,
		},
		{
			// Deliberately not a blanket "already exists" match: only a
			// constraint that is already there is the race this tolerates.
			name: "some other object already exists",
			err:  errors.New(`pq: relation "orders" already exists`),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDuplicateConstraintError(tc.err); got != tc.want {
				t.Errorf("isDuplicateConstraintError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
