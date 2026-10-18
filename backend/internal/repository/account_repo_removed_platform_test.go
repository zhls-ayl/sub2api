package repository

import (
	"context"
	"strconv"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/DATA-DOG/go-sqlmock"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

// removedPlatform was once registered (old migrations still allow it in the
// accounts/groups CHECK constraints) but no longer exists in domain.Platforms.
const removedPlatform = "adobe"

// knownPlatformPredicate binds exactly the registered concrete platforms, so a
// leftover row on a removed platform can never satisfy it.
func TestKnownPlatformPredicateExcludesRemovedPlatform(t *testing.T) {
	require.False(t, domain.IsConcretePlatform(removedPlatform))

	selector := entsql.Dialect(dialect.Postgres).Select("*").From(entsql.Table("accounts"))
	knownPlatformPredicate()(selector)
	query, args := selector.Query()

	require.Contains(t, query, `"platform" IN`)
	bound := make([]string, 0, len(args))
	for _, arg := range args {
		text, ok := arg.(string)
		require.True(t, ok)
		bound = append(bound, text)
	}
	require.ElementsMatch(t, domain.ConcretePlatformIDs(), bound)
	require.NotContains(t, bound, removedPlatform)
}

// Every schedulable query carries the known-platform predicate, so leftover
// rows are neither scheduled nor fed into snapshot rebuilds. A removed-platform
// lookup returns an empty list rather than an error.
func TestSchedulableQueriesApplyKnownPlatformPredicate(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		run  func(*accountRepository) (int, error)
	}{
		{"ListSchedulable", func(r *accountRepository) (int, error) {
			got, err := r.ListSchedulable(ctx)
			return len(got), err
		}},
		{"ListSchedulableByPlatform", func(r *accountRepository) (int, error) {
			got, err := r.ListSchedulableByPlatform(ctx, removedPlatform)
			return len(got), err
		}},
		{"ListSchedulableByPlatforms", func(r *accountRepository) (int, error) {
			got, err := r.ListSchedulableByPlatforms(ctx, []string{removedPlatform})
			return len(got), err
		}},
		{"ListSchedulableUngroupedByPlatform", func(r *accountRepository) (int, error) {
			got, err := r.ListSchedulableUngroupedByPlatform(ctx, removedPlatform)
			return len(got), err
		}},
		{"ListSchedulableUngroupedByPlatforms", func(r *accountRepository) (int, error) {
			got, err := r.ListSchedulableUngroupedByPlatforms(ctx, []string{removedPlatform})
			return len(got), err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			t.Cleanup(func() { _ = client.Close() })
			repo := newAccountRepositoryWithSQL(client, db, nil)

			// One IN placeholder per registered platform.
			mock.ExpectQuery(`"accounts"\."platform" IN \(\$\d+(, \$\d+){` + strconv.Itoa(len(domain.ConcretePlatformIDs())-1) + `}\)`).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))

			n, err := tc.run(repo)
			require.NoError(t, err)
			require.Zero(t, n)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
