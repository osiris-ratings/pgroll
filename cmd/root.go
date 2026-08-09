// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/xataio/pgroll/cmd/flags"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

// Version is the pgroll version
var Version = "development"

func NewRoll(ctx context.Context) (*roll.Roll, error) {
	pgURL := flags.PostgresURL()
	schema := flags.Schema()
	stateSchema := flags.StateSchema()
	lockTimeout := flags.LockTimeout()
	lockRetryTimeout := flags.LockRetryTimeout()
	role := flags.Role()
	skipValidation := flags.SkipValidation()
	verbose := flags.Verbose()
	useVersionSchema := flags.UseVersionSchema()
	target := flags.Target()

	state, err := state.New(ctx, pgURL, stateSchema, state.WithPgrollVersion(Version))
	if err != nil {
		return nil, err
	}

	return roll.New(
		ctx, pgURL, schema, state,
		roll.WithLockTimeoutMs(lockTimeout),
		roll.WithLockRetryTimeout(lockRetryTimeout),
		roll.WithRole(role),
		roll.WithSkipValidation(skipValidation),
		roll.WithLogging(verbose),
		roll.WithVersionSchema(useVersionSchema),
		roll.WithTarget(target),
		// Reversibility by construction: the CLI always requires migrations
		// to be revertible (or explicitly marked `irreversible: true`) so
		// `pgroll revert` can walk back out of any applied migration.
		roll.WithRequireReversible(),
	)
}

// EnsureInitialized checks if the pgroll state schema is initialized.
// Returns an error if the check fails or if pgroll is not initialized.
func EnsureInitialized(ctx context.Context, state *state.State) error {
	ok, err := state.IsInitialized(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errPGRollNotInitialized
	}
	return nil
}

// NewRollWithInitCheck creates a roll instance and checks if pgroll is initialized.
// Returns the roll instance and an error if creation fails or if pgroll is not initialized.
func NewRollWithInitCheck(ctx context.Context) (*roll.Roll, error) {
	// Create a roll instance
	m, err := NewRoll(ctx)
	if err != nil {
		return nil, err
	}

	// Ensure that pgroll is initialized
	if err := EnsureInitialized(ctx, m.State()); err != nil {
		m.Close()
		return nil, err
	}

	return m, nil
}

func Prepare() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:          "pgroll",
		SilenceUsage: true,
		Version:      Version,
		Long:         "For more information, visit http://pgroll.com/docs",
	}

	viper.SetEnvPrefix("PGROLL")
	viper.AutomaticEnv()

	rootCmd.PersistentFlags().String("postgres-url", "postgres://postgres:postgres@localhost?sslmode=disable", "Postgres URL")
	rootCmd.PersistentFlags().String("schema", "public", "Postgres schema to use for the migration")
	rootCmd.PersistentFlags().String("pgroll-schema", "pgroll", "Postgres schema to use for pgroll internal state")
	rootCmd.PersistentFlags().Int("lock-timeout", 500, "Postgres lock timeout in milliseconds for pgroll DDL operations")
	rootCmd.PersistentFlags().Duration("lock-retry-timeout", 5*time.Minute, "Total wall-clock budget for retrying lock_timeout errors before giving up (e.g. 5m, 30s); negative disables retries")
	rootCmd.PersistentFlags().String("role", "", "Optional postgres role to set when executing migrations")
	rootCmd.PersistentFlags().Bool("use-version-schema", true, "Create version schemas for each migration")
	rootCmd.PersistentFlags().String("target", "", "Only apply migrations whose `targets` include this name; unset applies every migration")
	rootCmd.PersistentFlags().Bool("verbose", false, "Enable verbose logging")

	viper.BindPFlag("PG_URL", rootCmd.PersistentFlags().Lookup("postgres-url"))
	viper.BindPFlag("SCHEMA", rootCmd.PersistentFlags().Lookup("schema"))
	viper.BindPFlag("STATE_SCHEMA", rootCmd.PersistentFlags().Lookup("pgroll-schema"))
	viper.BindPFlag("LOCK_TIMEOUT", rootCmd.PersistentFlags().Lookup("lock-timeout"))
	viper.BindPFlag("LOCK_RETRY_TIMEOUT", rootCmd.PersistentFlags().Lookup("lock-retry-timeout"))
	viper.BindPFlag("ROLE", rootCmd.PersistentFlags().Lookup("role"))
	viper.BindPFlag("USE_VERSION_SCHEMA", rootCmd.PersistentFlags().Lookup("use-version-schema"))
	viper.BindPFlag("TARGET", rootCmd.PersistentFlags().Lookup("target"))
	viper.BindPFlag("VERBOSE", rootCmd.PersistentFlags().Lookup("verbose"))

	// register subcommands
	rootCmd.AddCommand(startCmd())
	rootCmd.AddCommand(completeCmd())
	rootCmd.AddCommand(rollbackCmd)
	rootCmd.AddCommand(analyzeCmd)
	rootCmd.AddCommand(initCmd)
	rootCmd.AddCommand(statusCmd)
	rootCmd.AddCommand(updateCmd())
	rootCmd.AddCommand(createCmd())
	rootCmd.AddCommand(migrateCmd())
	rootCmd.AddCommand(planCmd())
	rootCmd.AddCommand(pullCmd())
	rootCmd.AddCommand(latestCmd())
	rootCmd.AddCommand(convertCmd())
	rootCmd.AddCommand(baselineCmd())
	rootCmd.AddCommand(materializeCmd())
	rootCmd.AddCommand(stampCmd())
	rootCmd.AddCommand(pruneCmd())
	rootCmd.AddCommand(revertCmd())
	rootCmd.AddCommand(validateCmd)
	rootCmd.AddCommand(checkCmd())

	return rootCmd
}

// Execute executes the root command.
//
// The root context is cancelled on SIGINT or SIGTERM so in-flight retry loops
// (see pkg/db.RDB) unwind cleanly and deferred rollback paths run before the
// process exits. A second signal after stop() is invoked restores default
// behavior, allowing a hard kill.
func Execute() error {
	// Let migration-file parse errors name the binary that rejected the file.
	// Strict decoding makes every new migration-file field a binary lockstep,
	// and "unknown field" alone does not tell an operator to upgrade.
	migrations.BinaryVersion = Version

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd := Prepare()
	return cmd.ExecuteContext(ctx)
}
