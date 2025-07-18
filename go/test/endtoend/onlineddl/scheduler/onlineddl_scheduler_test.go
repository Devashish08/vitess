/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduler

import (
	"context"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/mysql/capabilities"
	"vitess.io/vitess/go/textutil"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/schema"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/utils"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/throttle/throttlerapp"

	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/test/endtoend/onlineddl"
	"vitess.io/vitess/go/test/endtoend/throttler"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	anyErrorIndicator = "<any-error-of-any-kind>"
)

type testOnlineDDLStatementParams struct {
	ddlStatement     string
	ddlStrategy      string
	executeStrategy  string
	expectHint       string
	expectError      string
	skipWait         bool
	migrationContext string
}

type testRevertMigrationParams struct {
	revertUUID       string
	executeStrategy  string
	ddlStrategy      string
	migrationContext string
	expectError      string
	skipWait         bool
}

var (
	clusterInstance *cluster.LocalProcessCluster
	primaryTablet   *cluster.Vttablet
	shards          []cluster.Shard
	vtParams        mysql.ConnParams

	normalWaitTime            = 20 * time.Second
	extendedWaitTime          = 60 * time.Second
	ensureStateNotChangedTime = 5 * time.Second

	hostname              = "localhost"
	keyspaceName          = "ks"
	cell                  = "zone1"
	schemaChangeDirectory = ""
	overrideVtctlParams   *cluster.ApplySchemaParams
)

type WriteMetrics struct {
	mu                                                      sync.Mutex
	insertsAttempts, insertsFailures, insertsNoops, inserts int64
	updatesAttempts, updatesFailures, updatesNoops, updates int64
	deletesAttempts, deletesFailures, deletesNoops, deletes int64
}

func (w *WriteMetrics) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.inserts = 0
	w.updates = 0
	w.deletes = 0

	w.insertsAttempts = 0
	w.insertsFailures = 0
	w.insertsNoops = 0

	w.updatesAttempts = 0
	w.updatesFailures = 0
	w.updatesNoops = 0

	w.deletesAttempts = 0
	w.deletesFailures = 0
	w.deletesNoops = 0
}

func (w *WriteMetrics) String() string {
	return fmt.Sprintf(`WriteMetrics: inserts-deletes=%d, updates-deletes=%d,
insertsAttempts=%d, insertsFailures=%d, insertsNoops=%d, inserts=%d,
updatesAttempts=%d, updatesFailures=%d, updatesNoops=%d, updates=%d,
deletesAttempts=%d, deletesFailures=%d, deletesNoops=%d, deletes=%d,
`,
		w.inserts-w.deletes, w.updates-w.deletes,
		w.insertsAttempts, w.insertsFailures, w.insertsNoops, w.inserts,
		w.updatesAttempts, w.updatesFailures, w.updatesNoops, w.updates,
		w.deletesAttempts, w.deletesFailures, w.deletesNoops, w.deletes,
	)
}

func parseTableName(t *testing.T, sql string) (tableName string) {
	// ddlStatement could possibly be composed of multiple DDL statements
	parser := sqlparser.NewTestParser()
	stmts, err := parser.ParseMultiple(sql)
	require.NoError(t, err)
	for _, stmt := range stmts {
		ddlStmt, ok := stmt.(sqlparser.DDLStatement)
		require.True(t, ok)
		tableName = ddlStmt.GetTable().Name.String()
		if tableName == "" {
			tbls := ddlStmt.AffectedTables()
			require.NotEmpty(t, tbls)
			tableName = tbls[0].Name.String()
		}
		require.NotEmptyf(t, tableName, "could not parse table name from SQL: %s", sqlparser.String(ddlStmt))
	}
	require.NotEmptyf(t, tableName, "could not parse table name from SQL: %s", sql)
	return tableName
}

// testOnlineDDLStatement runs an online DDL, ALTER statement
func TestParseTableName(t *testing.T) {
	sqls := []string{
		`ALTER TABLE t1_test ENGINE=InnoDB`,
		`ALTER TABLE t1_test ENGINE=InnoDB;`,
		`DROP TABLE IF EXISTS t1_test`,
		`
			ALTER TABLE stress_test ENGINE=InnoDB;
			ALTER TABLE stress_test ENGINE=InnoDB;
			ALTER TABLE stress_test ENGINE=InnoDB;
		`,
	}

	for _, sql := range sqls {
		t.Run(sql, func(t *testing.T) {
			parseTableName(t, sql)
		})
	}
}

func waitForReadyToComplete(t *testing.T, uuid string, expected bool) {
	ctx, cancel := context.WithTimeout(context.Background(), normalWaitTime)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {

		rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			readyToComplete := row.AsInt64("ready_to_complete", 0)
			if expected == (readyToComplete > 0) {
				// all good. This is what we waited for
				if expected {
					// if migration is ready to complete, the nthe timestamp should be non-null
					assert.False(t, row["ready_to_complete_timestamp"].IsNull())
				} else {
					assert.True(t, row["ready_to_complete_timestamp"].IsNull())
				}

				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
		require.NoError(t, ctx.Err(), "waiting for ready_to_complete=%t for %v", expected, uuid)
	}
}

func waitForMessage(t *testing.T, uuid string, messageSubstring string) {
	ctx, cancel := context.WithTimeout(context.Background(), normalWaitTime)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var lastMessage string
	for {
		rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			lastMessage = row.AsString("message", "")
			if strings.Contains(lastMessage, messageSubstring) {
				return
			}
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			{
				resp, err := throttler.CheckThrottler(&clusterInstance.VtctldClientProcess, primaryTablet, throttlerapp.TestingName, nil)
				assert.NoError(t, err)
				fmt.Println("Throttler check response: ", resp)

				output, err := throttler.GetThrottlerStatusRaw(&clusterInstance.VtctldClientProcess, primaryTablet)
				assert.NoError(t, err)
				fmt.Println("Throttler status response: ", output)
			}
			require.Failf(t, "timeout waiting for message", "expected: %s. Last seen: %s", messageSubstring, lastMessage)
			return
		}
	}
}

func TestMain(m *testing.M) {
	flag.Parse()

	exitcode, err := func() (int, error) {
		clusterInstance = cluster.NewCluster(cell, hostname)
		schemaChangeDirectory = path.Join("/tmp", fmt.Sprintf("schema_change_dir_%d", clusterInstance.GetAndReserveTabletUID()))
		defer os.RemoveAll(schemaChangeDirectory)
		defer clusterInstance.Teardown()

		if _, err := os.Stat(schemaChangeDirectory); os.IsNotExist(err) {
			_ = os.Mkdir(schemaChangeDirectory, 0700)
		}

		clusterInstance.VtctldExtraArgs = []string{
			"--schema-change-dir", schemaChangeDirectory,
			"--schema-change-controller", "local",
			"--schema-change-check-interval", "1s",
		}

		clusterInstance.VtTabletExtraArgs = []string{
			utils.GetFlagVariantForTests("--heartbeat-interval"), "250ms",
			utils.GetFlagVariantForTests("--heartbeat-on-demand-duration"), "5s",
			utils.GetFlagVariantForTests("--migration-check-interval"), "2s",
			utils.GetFlagVariantForTests("--watch-replication-stream"),
		}
		clusterInstance.VtGateExtraArgs = []string{}

		if err := clusterInstance.StartTopo(); err != nil {
			return 1, err
		}

		// Start keyspace
		keyspace := &cluster.Keyspace{
			Name: keyspaceName,
		}

		// No need for replicas in this stress test
		if err := clusterInstance.StartKeyspace(*keyspace, []string{"1"}, 0, false); err != nil {
			return 1, err
		}

		vtgateInstance := clusterInstance.NewVtgateInstance()
		// Start vtgate
		if err := vtgateInstance.Setup(); err != nil {
			return 1, err
		}
		// ensure it is torn down during cluster TearDown
		clusterInstance.VtgateProcess = *vtgateInstance
		vtParams = mysql.ConnParams{
			Host: clusterInstance.Hostname,
			Port: clusterInstance.VtgateMySQLPort,
		}
		primaryTablet = clusterInstance.Keyspaces[0].Shards[0].PrimaryTablet()

		return m.Run(), nil
	}()
	if err != nil {
		fmt.Printf("%v\n", err)
		os.Exit(1)
	} else {
		os.Exit(exitcode)
	}

}

func TestSchedulerSchemaChanges(t *testing.T) {

	throttler.EnableLagThrottlerAndWaitForStatus(t, clusterInstance)

	t.Run("scheduler", testScheduler)
	t.Run("singleton", testSingleton)
	t.Run("declarative", testDeclarative)
	t.Run("foreign-keys", testForeignKeys)
	t.Run("summary: validate sequential migration IDs", func(t *testing.T) {
		onlineddl.ValidateSequentialMigrationIDs(t, &vtParams, shards)
	})
	t.Run("summary: validate completed_timestamp", func(t *testing.T) {
		onlineddl.ValidateCompletedTimestamp(t, &vtParams)
	})
}

func testScheduler(t *testing.T) {
	shards = clusterInstance.Keyspaces[0].Shards
	require.Equal(t, 1, len(shards))

	ddlStrategy := "vitess"

	createParams := func(ddlStatement string, ddlStrategy string, executeStrategy string, expectHint string, expectError string, skipWait bool) *testOnlineDDLStatementParams {
		return &testOnlineDDLStatementParams{
			ddlStatement:    ddlStatement,
			ddlStrategy:     ddlStrategy,
			executeStrategy: executeStrategy,
			expectHint:      expectHint,
			expectError:     expectError,
			skipWait:        skipWait,
		}
	}

	createRevertParams := func(revertUUID string, ddlStrategy string, executeStrategy string, expectError string, skipWait bool) *testRevertMigrationParams {
		return &testRevertMigrationParams{
			revertUUID:      revertUUID,
			executeStrategy: executeStrategy,
			ddlStrategy:     ddlStrategy,
			expectError:     expectError,
			skipWait:        skipWait,
		}
	}

	mysqlVersion := onlineddl.GetMySQLVersion(t, primaryTablet)
	require.NotEmpty(t, mysqlVersion)
	capableOf := mysql.ServerVersionCapableOf(mysqlVersion)

	var (
		t1uuid string
		t2uuid string

		t1Name            = "t1_test"
		t2Name            = "t2_test"
		createT1Statement = `
			CREATE TABLE t1_test (
				id bigint(20) not null,
				hint_col varchar(64) not null default 'just-created',
				PRIMARY KEY (id)
			) ENGINE=InnoDB
		`
		createT2Statement = `
			CREATE TABLE t2_test (
				id bigint(20) not null,
				hint_col varchar(64) not null default 'just-created',
				PRIMARY KEY (id)
			) ENGINE=InnoDB
		`
		createT1IfNotExistsStatement = `
			CREATE TABLE IF NOT EXISTS t1_test (
				id bigint(20) not null,
				hint_col varchar(64) not null default 'should_not_appear',
				PRIMARY KEY (id)
			) ENGINE=InnoDB
		`
		trivialAlterT1Statement = `
			ALTER TABLE t1_test ENGINE=InnoDB;
		`
		trivialAlterT2Statement = `
			ALTER TABLE t2_test ENGINE=InnoDB;
		`
		instantAlterT1Statement = `
			ALTER TABLE t1_test ADD COLUMN i0 INT NOT NULL DEFAULT 0
		`
		instantUndoAlterT1Statement = `
			ALTER TABLE t1_test DROP COLUMN i0
		`
		dropT1Statement = `
			DROP TABLE IF EXISTS t1_test
		`
		dropT3Statement = `
			DROP TABLE IF EXISTS t3_test
		`
		dropT4Statement = `
			DROP TABLE IF EXISTS t4_test
		`
		alterExtraColumn = `
			ALTER TABLE t1_test ADD COLUMN extra_column int NOT NULL DEFAULT 0
		`
		createViewDependsOnExtraColumn = `
			CREATE VIEW t1_test_view AS SELECT id, extra_column FROM t1_test
		`
		alterNonexistent = `
				ALTER TABLE nonexistent FORCE
		`
		populateT1Statement = `
			insert ignore into t1_test values (1, 'new_row')
		`
	)

	testReadTimestamp := func(t *testing.T, uuid string, timestampColumn string) (timestamp string) {
		rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			timestamp = row.AsString(timestampColumn, "")
			assert.NotEmpty(t, timestamp)
		}
		return timestamp
	}
	testTableSequentialTimes := func(t *testing.T, uuid1, uuid2 string) {
		// expect uuid2 to start after uuid1 completes
		t.Run("Compare t1, t2 sequential times", func(t *testing.T) {
			endTime1 := testReadTimestamp(t, uuid1, "completed_timestamp")
			startTime2 := testReadTimestamp(t, uuid2, "started_timestamp")
			assert.GreaterOrEqual(t, startTime2, endTime1)
		})
	}
	testTableCompletionTimes := func(t *testing.T, uuid1, uuid2 string) {
		// expect uuid1 to complete before uuid2
		t.Run("Compare t1, t2 completion times", func(t *testing.T) {
			endTime1 := testReadTimestamp(t, uuid1, "completed_timestamp")
			endTime2 := testReadTimestamp(t, uuid2, "completed_timestamp")
			assert.GreaterOrEqual(t, endTime2, endTime1)
		})
	}
	testTableCompletionAndStartTimes := func(t *testing.T, uuid1, uuid2 string) {
		// expect uuid1 to complete before uuid2
		t.Run("Compare t1, t2 completion times", func(t *testing.T) {
			endTime1 := testReadTimestamp(t, uuid1, "completed_timestamp")
			startedTime2 := testReadTimestamp(t, uuid2, "started_timestamp")
			assert.GreaterOrEqual(t, startedTime2, endTime1)
		})
	}
	testAllowConcurrent := func(t *testing.T, name string, uuid string, expect int64) {
		t.Run("verify allow_concurrent: "+name, func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				allowConcurrent := row.AsInt64("allow_concurrent", 0)
				assert.Equal(t, expect, allowConcurrent)
			}
		})
	}

	var originalLockWaitTimeout int64
	t.Run("set low lock_wait_timeout", func(t *testing.T) {
		rs, err := primaryTablet.VttabletProcess.QueryTablet("select @@lock_wait_timeout as lock_wait_timeout", keyspaceName, false)
		require.NoError(t, err)
		row := rs.Named().Row()
		require.NotNil(t, row)
		originalLockWaitTimeout = row.AsInt64("lock_wait_timeout", 0)
		require.NotZero(t, originalLockWaitTimeout)

		_, err = primaryTablet.VttabletProcess.QueryTablet("set global lock_wait_timeout=1", keyspaceName, false)
		require.NoError(t, err)
	})

	// CREATE
	t.Run("CREATE TABLEs t1, t2", func(t *testing.T) {
		{ // The table does not exist
			t1uuid = testOnlineDDLStatement(t, createParams(createT1Statement, ddlStrategy, "vtgate", "just-created", "", false))
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t1Name, true)
		}
		{
			// The table does not exist
			t2uuid = testOnlineDDLStatement(t, createParams(createT2Statement, ddlStrategy, "vtgate", "just-created", "", false))
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t2Name, true)
		}
		testTableSequentialTimes(t, t1uuid, t2uuid)
	})
	t.Run("Postpone launch CREATE", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(createT1IfNotExistsStatement, ddlStrategy+" --postpone-launch --cut-over-threshold=14s", "vtgate", "", "", true)) // skip wait
		time.Sleep(2 * time.Second)
		rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			postponeLaunch := row.AsInt64("postpone_launch", 0)
			assert.Equal(t, int64(1), postponeLaunch)
		}
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusQueued)

		t.Run("launch all shards", func(t *testing.T) {
			onlineddl.CheckLaunchMigration(t, &vtParams, shards, t1uuid, "", true)
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeLaunch := row.AsInt64("postpone_launch", 0)
				assert.Equal(t, int64(0), postponeLaunch)

				cutOverThresholdSeconds := row.AsInt64("cutover_threshold_seconds", 0)
				// Threshold supplied in DDL strategy
				assert.EqualValues(t, 14, cutOverThresholdSeconds)
			}
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
	})
	t.Run("Postpone launch ALTER", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-launch", "vtgate", "", "", true)) // skip wait
		time.Sleep(2 * time.Second)
		rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			postponeLaunch := row.AsInt64("postpone_launch", 0)
			assert.Equal(t, int64(1), postponeLaunch)
		}
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusQueued)

		t.Run("launch irrelevant UUID", func(t *testing.T) {
			someOtherUUID := "00000000_1111_2222_3333_444444444444"
			onlineddl.CheckLaunchMigration(t, &vtParams, shards, someOtherUUID, "", false)
			time.Sleep(2 * time.Second)
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeLaunch := row.AsInt64("postpone_launch", 0)
				assert.Equal(t, int64(1), postponeLaunch)
			}
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusQueued)
		})
		t.Run("launch irrelevant shards", func(t *testing.T) {
			onlineddl.CheckLaunchMigration(t, &vtParams, shards, t1uuid, "x,y,z", false)
			time.Sleep(2 * time.Second)
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeLaunch := row.AsInt64("postpone_launch", 0)
				assert.Equal(t, int64(1), postponeLaunch)
			}
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusQueued)
		})
		t.Run("launch relevant shard", func(t *testing.T) {
			onlineddl.CheckLaunchMigration(t, &vtParams, shards, t1uuid, "x, y, 1", true)
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeLaunch := row.AsInt64("postpone_launch", 0)
				assert.Equal(t, int64(0), postponeLaunch)
			}
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
	})

	t.Run("Postpone completion ALTER", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion", "vtgate", "", "", true)) // skip wait

		t.Run("wait for t1 running", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		})

		t.Run("wait for ready_to_complete", func(t *testing.T) {
			waitForReadyToComplete(t, t1uuid, true)
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				assert.True(t, row["shadow_analyzed_timestamp"].IsNull())
			}
		})

		t.Run("check postpone_completion", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.Equal(t, int64(1), postponeCompletion)
			}
		})
		t.Run("set cut-over threshold", func(t *testing.T) {
			onlineddl.CheckSetMigrationCutOverThreshold(t, &vtParams, shards, t1uuid, 17700*time.Millisecond, "")
		})
		t.Run("complete", func(t *testing.T) {
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("check no postpone_completion", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.Equal(t, int64(0), postponeCompletion)

				cutOverThresholdSeconds := row.AsInt64("cutover_threshold_seconds", 0)
				// Expect 17800*time.Millisecond to be truncated to 17 seconds
				assert.EqualValues(t, 17, cutOverThresholdSeconds)

				assert.False(t, row["shadow_analyzed_timestamp"].IsNull())
			}
		})
	})

	t.Run("Postpone completion ALTER with shards", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion", "vtgate", "", "", true)) // skip wait

		t.Run("wait for t1 running", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		})

		t.Run("wait for ready_to_complete", func(t *testing.T) {
			waitForReadyToComplete(t, t1uuid, true)
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				assert.True(t, row["shadow_analyzed_timestamp"].IsNull())
			}
		})

		t.Run("check postpone_completion", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.Equal(t, int64(1), postponeCompletion)
			}
		})
		t.Run("complete with irrelevant shards", func(t *testing.T) {
			onlineddl.CheckCompleteMigrationShards(t, &vtParams, shards, t1uuid, "x,y,z", false)
			// Added an artificial sleep here just to ensure we're not missing a would-be completion./
			time.Sleep(2 * time.Second)
			// Migration should still be in running state
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// postpone_completion should still be set
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.Equal(t, int64(1), postponeCompletion)
			}
		})
		t.Run("complete with relevant shards", func(t *testing.T) {
			onlineddl.CheckCompleteMigrationShards(t, &vtParams, shards, t1uuid, "x, y, 1", true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("check no postpone_completion", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.Equal(t, int64(0), postponeCompletion)
			}
		})
	})

	t.Run("Delayed postpone completion ALTER", func(t *testing.T) {
		onlineddl.ThrottleAllMigrations(t, &vtParams)
		defer onlineddl.UnthrottleAllMigrations(t, &vtParams)
		onlineddl.CheckThrottledApps(t, &vtParams, throttlerapp.OnlineDDLName, true)

		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy, "vtgate", "", "", true)) // skip wait

		t.Run("wait for t1 running", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		})

		t.Run("check postpone_completion", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.EqualValues(t, 0, postponeCompletion)
			}
		})
		t.Run("postpone", func(t *testing.T) {
			onlineddl.CheckPostponeCompleteMigration(t, &vtParams, shards, t1uuid, true)
		})
		t.Run("check postpone_completion set", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.EqualValues(t, 1, postponeCompletion)
			}
		})
		onlineddl.UnthrottleAllMigrations(t, &vtParams)
		onlineddl.CheckThrottledApps(t, &vtParams, throttlerapp.OnlineDDLName, false)

		t.Run("wait for ready_to_complete", func(t *testing.T) {
			waitForReadyToComplete(t, t1uuid, true)
		})
		t.Run("complete", func(t *testing.T) {
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("check no postpone_completion", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				postponeCompletion := row.AsInt64("postpone_completion", 0)
				assert.EqualValues(t, 0, postponeCompletion)
			}
		})
	})

	t.Run("show vitess_migrations in transaction", func(t *testing.T) {
		// The function validates there is no error
		rs := onlineddl.VtgateExecQueryInTransaction(t, &vtParams, "show vitess_migrations", "")
		assert.NotEmpty(t, rs.Rows)
	})

	t.Run("low @@lock_wait_timeout", func(t *testing.T) {
		defer primaryTablet.VttabletProcess.QueryTablet(fmt.Sprintf("set global lock_wait_timeout=%d", originalLockWaitTimeout), keyspaceName, false)

		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy, "vtgate", "", "", false)) // wait
		t.Run("trivial t1 migration", func(t *testing.T) {
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t1Name, true)
		})
	})

	forceCutoverCapable, err := capableOf(capabilities.PerformanceSchemaDataLocksTableCapability) // 8.0
	require.NoError(t, err)
	if forceCutoverCapable {
		t.Run("force_cutover", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), extendedWaitTime*5)
			defer cancel()

			t.Run("populate t1_test", func(t *testing.T) {
				onlineddl.VtgateExecQuery(t, &vtParams, populateT1Statement, "")
			})
			t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion", "vtgate", "", "", true)) // skip wait

			t.Run("wait for t1 running", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			})
			t.Run("wait for t1 ready to complete", func(t *testing.T) {
				// Waiting for 'running', above, is not enough. We want to let vreplication a chance to start running, or else
				// we attempt the cut-over too early. Specifically in this test, we're going to lock rows FOR UPDATE, which,
				// if vreplication does not get the chance to start, will prevent it from doing anything at all.
				// ready_to_complete is a great signal for us that vreplication is healthy and up to date.
				waitForReadyToComplete(t, t1uuid, true)
			})

			commitTransactionChan := make(chan any)
			transactionErrorChan := make(chan error)
			t.Run("locking table rows", func(t *testing.T) {
				go runInTransaction(t, ctx, primaryTablet, "select * from t1_test for update", commitTransactionChan, transactionErrorChan)
			})
			t.Run("injecting heartbeats asynchronously", func(t *testing.T) {
				go func() {
					ticker := time.NewTicker(time.Second)
					defer ticker.Stop()
					for {
						throttler.CheckThrottler(&clusterInstance.VtctldClientProcess, primaryTablet, throttlerapp.OnlineDDLName, nil)
						select {
						case <-ticker.C:
						case <-ctx.Done():
							return
						}
					}
				}()
			})
			t.Run("check no force_cutover", func(t *testing.T) {
				rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
				require.NotNil(t, rs)
				for _, row := range rs.Named().Rows {
					forceCutOver := row.AsInt64("force_cutover", 0)
					assert.Equal(t, int64(0), forceCutOver) // disabled
				}
			})
			t.Run("attempt to complete", func(t *testing.T) {
				onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			})
			t.Run("cut-over fail due to timeout", func(t *testing.T) {
				waitForMessage(t, t1uuid, "(errno 3024) (sqlstate HY000): Query execution was interrupted, maximum statement execution time exceeded")
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusRunning)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			})
			t.Run("force_cutover", func(t *testing.T) {
				onlineddl.CheckForceMigrationCutOver(t, &vtParams, shards, t1uuid, true)
			})
			t.Run("check force_cutover", func(t *testing.T) {
				rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
				require.NotNil(t, rs)
				for _, row := range rs.Named().Rows {
					forceCutOver := row.AsInt64("force_cutover", 0)
					assert.Equal(t, int64(1), forceCutOver) // enabled
				}
			})
			t.Run("expect completion", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			})
			t.Run("expect transaction failure", func(t *testing.T) {
				select {
				case commitTransactionChan <- true: // good
				case <-ctx.Done():
					assert.Fail(t, ctx.Err().Error())
				}
				// Transaction will now attempt to commit. But we expect our "force_cutover" to have terminated
				// the transaction's connection.
				select {
				case err := <-transactionErrorChan:
					assert.ErrorContains(t, err, "broken pipe")
				case <-ctx.Done():
					assert.Fail(t, ctx.Err().Error())
				}
			})
		})
		t.Run("force_cutover mdl", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), extendedWaitTime*5)
			defer cancel()

			t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion", "vtgate", "", "", true)) // skip wait

			t.Run("wait for t1 running", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			})
			t.Run("wait for t1 ready to complete", func(t *testing.T) {
				// Waiting for 'running', above, is not enough. We want to let vreplication a chance to start running, or else
				// we attempt the cut-over too early. Specifically in this test, we're going to lock rows FOR UPDATE, which,
				// if vreplication does not get the chance to start, will prevent it from doing anything at all.
				// ready_to_complete is a great signal for us that vreplication is healthy and up to date.
				waitForReadyToComplete(t, t1uuid, true)
			})

			conn, err := primaryTablet.VttabletProcess.TabletConn(keyspaceName, true)
			require.NoError(t, err)
			defer conn.Close()

			unlockTables := func() error {
				_, err := conn.ExecuteFetch("unlock tables", 0, false)
				return err
			}
			t.Run("locking table", func(t *testing.T) {
				_, err := conn.ExecuteFetch("lock tables t1_test write", 0, false)
				require.NoError(t, err)
			})
			defer unlockTables()
			t.Run("injecting heartbeats asynchronously", func(t *testing.T) {
				go func() {
					ticker := time.NewTicker(time.Second)
					defer ticker.Stop()
					for {
						throttler.CheckThrottler(&clusterInstance.VtctldClientProcess, primaryTablet, throttlerapp.OnlineDDLName, nil)
						select {
						case <-ticker.C:
						case <-ctx.Done():
							return
						}
					}
				}()
			})
			t.Run("check no force_cutover", func(t *testing.T) {
				rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
				require.NotNil(t, rs)
				for _, row := range rs.Named().Rows {
					forceCutOver := row.AsInt64("force_cutover", 0)
					assert.Equal(t, int64(0), forceCutOver) // disabled
				}
			})
			t.Run("attempt to complete", func(t *testing.T) {
				onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			})
			t.Run("cut-over fail due to timeout", func(t *testing.T) {
				waitForMessage(t, t1uuid, "(errno 3024) (sqlstate HY000): Query execution was interrupted, maximum statement execution time exceeded")
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusRunning)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			})
			t.Run("force_cutover", func(t *testing.T) {
				onlineddl.CheckForceMigrationCutOver(t, &vtParams, shards, t1uuid, true)
			})
			t.Run("check force_cutover", func(t *testing.T) {
				rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
				require.NotNil(t, rs)
				for _, row := range rs.Named().Rows {
					forceCutOver := row.AsInt64("force_cutover", 0)
					assert.Equal(t, int64(1), forceCutOver) // enabled
				}
			})
			t.Run("expect completion", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			})
			t.Run("expect unlock failure", func(t *testing.T) {
				err := unlockTables()
				assert.ErrorContains(t, err, "broken pipe")
			})
		})
	}

	if forceCutoverCapable {
		t.Run("force_cutover_instant", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), extendedWaitTime*5)
			defer cancel()

			t.Run("populate t1_test", func(t *testing.T) {
				onlineddl.VtgateExecQuery(t, &vtParams, populateT1Statement, "")
			})

			commitTransactionChan := make(chan any)
			transactionErrorChan := make(chan error)
			t.Run("locking table rows", func(t *testing.T) {
				go runInTransaction(t, ctx, primaryTablet, "select * from t1_test for update", commitTransactionChan, transactionErrorChan)
			})

			t.Run("execute migration", func(t *testing.T) {
				t1uuid = testOnlineDDLStatement(t, createParams(instantAlterT1Statement, ddlStrategy+" --prefer-instant-ddl --force-cut-over-after=1ms", "vtgate", "", "", true)) // skip wait
			})
			t.Run("expect completion", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			})
			t.Run("check special_plan", func(t *testing.T) {
				rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
				require.NotNil(t, rs)
				for _, row := range rs.Named().Rows {
					specialPlan := row.AsString("special_plan", "")
					assert.Contains(t, specialPlan, "instant-ddl")
				}
			})
			t.Run("expect transaction failure", func(t *testing.T) {
				select {
				case commitTransactionChan <- true: // good
				case <-ctx.Done():
					assert.Fail(t, ctx.Err().Error())
				}
				// Transaction will now attempt to commit. But we expect our "force_cutover" to have terminated
				// the transaction's connection.
				select {
				case err := <-transactionErrorChan:
					assert.ErrorContains(t, err, "broken pipe")
				case <-ctx.Done():
					assert.Fail(t, ctx.Err().Error())
				}
			})
			t.Run("cleanup: undo migration", func(t *testing.T) {
				t1uuid = testOnlineDDLStatement(t, createParams(instantUndoAlterT1Statement, ddlStrategy+" --prefer-instant-ddl --force-cut-over-after=1ms", "vtgate", "", "", true)) // skip wait
			})
			t.Run("cleanup: expect completion", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			})
		})
	}

	t.Run("ALTER both tables non-concurrent", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy, "vtgate", "", "", true)) // skip wait
		t2uuid = testOnlineDDLStatement(t, createParams(trivialAlterT2Statement, ddlStrategy, "vtgate", "", "", true)) // skip wait

		t.Run("wait for t1 complete", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		})
		t.Run("wait for t1 complete", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		})
		t.Run("check both complete", func(t *testing.T) {
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusComplete)
		})
		testTableSequentialTimes(t, t1uuid, t2uuid)
	})
	t.Run("ALTER both tables non-concurrent, postponed", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" -postpone-completion", "vtgate", "", "", true)) // skip wait
		t2uuid = testOnlineDDLStatement(t, createParams(trivialAlterT2Statement, ddlStrategy+" -postpone-completion", "vtgate", "", "", true)) // skip wait

		testAllowConcurrent(t, "t1", t1uuid, 0)
		t.Run("expect t1 running, t2 queued", func(t *testing.T) {
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			// now that t1 is running, let's unblock t2. We expect it to remain queued.
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t2uuid, true)
			time.Sleep(ensureStateNotChangedTime)
			// t1 should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// non-concurrent -- should be queued!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
		})
		t.Run("complete t1", func(t *testing.T) {
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("expect t2 to complete", func(t *testing.T) {
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusComplete)
		})
		testTableSequentialTimes(t, t1uuid, t2uuid)
	})

	t.Run("ALTER both tables, elligible for concurrent", func(t *testing.T) {
		// ALTER TABLE is allowed to run concurrently when no other ALTER is busy with copy state. Our tables are tiny so we expect to find both migrations running
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --allow-concurrent --postpone-completion", "vtgate", "", "", true)) // skip wait
		t2uuid = testOnlineDDLStatement(t, createParams(trivialAlterT2Statement, ddlStrategy+" --allow-concurrent --postpone-completion", "vtgate", "", "", true)) // skip wait

		testAllowConcurrent(t, "t1", t1uuid, 1)
		testAllowConcurrent(t, "t2", t2uuid, 1)
		t.Run("expect both running", func(t *testing.T) {
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			time.Sleep(ensureStateNotChangedTime)
			// both should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusRunning)
		})
		t.Run("complete t2", func(t *testing.T) {
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t2uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("complete t1", func(t *testing.T) {
			// t1 should still be running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		testTableCompletionTimes(t, t2uuid, t1uuid)
	})
	t.Run("ALTER both tables, elligible for concurrent, with throttling", func(t *testing.T) {
		onlineddl.ThrottleAllMigrations(t, &vtParams)
		defer onlineddl.UnthrottleAllMigrations(t, &vtParams)
		onlineddl.CheckThrottledApps(t, &vtParams, throttlerapp.OnlineDDLName, true)

		// ALTER TABLE is allowed to run concurrently when no other ALTER is busy with copy state. Our tables are tiny so we expect to find both migrations running
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --allow-concurrent --postpone-completion", "vtgate", "", "", true)) // skip wait
		t2uuid = testOnlineDDLStatement(t, createParams(trivialAlterT2Statement, ddlStrategy+" --allow-concurrent --postpone-completion", "vtgate", "", "", true)) // skip wait

		testAllowConcurrent(t, "t1", t1uuid, 1)
		testAllowConcurrent(t, "t2", t2uuid, 1)
		t.Run("expect t1 running", func(t *testing.T) {
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			// since all migrations are throttled, t1 migration is not ready_to_complete, hence
			// t2 should not be running
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
			time.Sleep(ensureStateNotChangedTime)
			// both should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)

			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				userThrotteRatio := row.AsFloat64("user_throttle_ratio", 0)
				assert.EqualValues(t, 1.0, userThrotteRatio)
			}
			// t2uuid migration is not in 'running' state, hence 'user_throttle_ratio' is not updated
			rs = onlineddl.ReadMigrations(t, &vtParams, t2uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				userThrotteRatio := row.AsFloat64("user_throttle_ratio", 0)
				assert.EqualValues(t, 0, userThrotteRatio)
			}
		})

		t.Run("check ready to complete (before)", func(t *testing.T) {
			for _, uuid := range []string{t1uuid, t2uuid} {
				waitForReadyToComplete(t, uuid, false)
			}
		})
		t.Run("unthrottle, expect t2 running", func(t *testing.T) {
			onlineddl.UnthrottleAllMigrations(t, &vtParams)
			// t1 should now be ready_to_complete, hence t2 should start running
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, extendedWaitTime, schema.OnlineDDLStatusRunning)
			time.Sleep(ensureStateNotChangedTime)
			// both should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusRunning)

			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				userThrotteRatio := row.AsFloat64("user_throttle_ratio", 0)
				assert.EqualValues(t, 0, userThrotteRatio)
			}
			rs = onlineddl.ReadMigrations(t, &vtParams, t2uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				userThrotteRatio := row.AsFloat64("user_throttle_ratio", 0)
				assert.EqualValues(t, 0, userThrotteRatio)
			}
		})
		t.Run("throttle t2", func(t *testing.T) {
			throttler.ThrottleAppAndWaitUntilTabletsConfirm(t, clusterInstance, throttlerapp.Name(t2uuid))
			time.Sleep(ensureStateNotChangedTime)
			rs := onlineddl.ReadMigrations(t, &vtParams, t2uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				userThrotteRatio := row.AsFloat64("user_throttle_ratio", 0)
				assert.EqualValues(t, 1.0, userThrotteRatio)
			}
		})
		t.Run("unthrottle t2", func(t *testing.T) {
			throttler.UnthrottleAppAndWaitUntilTabletsConfirm(t, clusterInstance, throttlerapp.Name(t2uuid))
			time.Sleep(ensureStateNotChangedTime)
			rs := onlineddl.ReadMigrations(t, &vtParams, t2uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				userThrotteRatio := row.AsFloat64("user_throttle_ratio", 0)
				assert.EqualValues(t, 0, userThrotteRatio)
			}
		})
		t.Run("complete t2", func(t *testing.T) {
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t2uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("complete t1", func(t *testing.T) {
			// t1 should still be running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("check ready to complete (after)", func(t *testing.T) {
			for _, uuid := range []string{t1uuid, t2uuid} {
				waitForReadyToComplete(t, uuid, true)
			}
		})

		testTableCompletionTimes(t, t2uuid, t1uuid)
	})
	onlineddl.CheckThrottledApps(t, &vtParams, throttlerapp.OnlineDDLName, false)

	t.Run("REVERT both tables concurrent, postponed", func(t *testing.T) {
		t1uuid = testRevertMigration(t, createRevertParams(t1uuid, ddlStrategy+" --allow-concurrent --postpone-completion", "vtgate", "", true))
		t2uuid = testRevertMigration(t, createRevertParams(t2uuid, ddlStrategy+" --allow-concurrent --postpone-completion", "vtgate", "", true))

		testAllowConcurrent(t, "t1", t1uuid, 1)
		t.Run("expect both migrations to run", func(t *testing.T) {
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
		})
		t.Run("test ready-to-complete", func(t *testing.T) {
			waitForReadyToComplete(t, t1uuid, true)
		})
		t.Run("complete t2", func(t *testing.T) {
			// now that both are running, let's unblock t2. We expect it to complete.
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t2uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t2uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t2uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("complete t1", func(t *testing.T) {
			// t1 should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
	})
	t.Run("concurrent REVERT vs two non-concurrent DROPs", func(t *testing.T) {
		t1uuid = testRevertMigration(t, createRevertParams(t1uuid, ddlStrategy+" -allow-concurrent -postpone-completion", "vtgate", "", true))
		drop3uuid := testOnlineDDLStatement(t, createParams(dropT3Statement, ddlStrategy, "vtgate", "", "", true)) // skip wait

		testAllowConcurrent(t, "t1", t1uuid, 1)
		testAllowConcurrent(t, "drop3", drop3uuid, 0)
		t.Run("expect t1 migration to run", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
		})
		drop1uuid := testOnlineDDLStatement(t, createParams(dropT1Statement, ddlStrategy, "vtgate", "", "", true)) // skip wait
		t.Run("drop3 complete", func(t *testing.T) {
			// drop3 migration should not block. It can run concurrently to t1, and does not conflict
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, drop3uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop3uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("drop1 blocked", func(t *testing.T) {
			// drop1 migration should block. It can run concurrently to t1, but conflicts on table name
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
			// let's cancel it
			onlineddl.CheckCancelMigration(t, &vtParams, shards, drop1uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, drop1uuid, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusCancelled)
		})
		t.Run("complete t1", func(t *testing.T) {
			// t1 should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
	})
	t.Run("non-concurrent REVERT vs three concurrent drops", func(t *testing.T) {
		t1uuid = testRevertMigration(t, createRevertParams(t1uuid, ddlStrategy+" -postpone-completion", "vtgate", "", true))
		drop3uuid := testOnlineDDLStatement(t, createParams(dropT3Statement, ddlStrategy+" -allow-concurrent", "vtgate", "", "", true))                      // skip wait
		drop4uuid := testOnlineDDLStatement(t, createParams(dropT4Statement, ddlStrategy+" -allow-concurrent -postpone-completion", "vtgate", "", "", true)) // skip wait

		testAllowConcurrent(t, "drop3", drop3uuid, 1)
		t.Run("expect t1 migration to run", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
		})
		drop1uuid := testOnlineDDLStatement(t, createParams(dropT1Statement, ddlStrategy+" -allow-concurrent", "vtgate", "", "", true)) // skip wait
		testAllowConcurrent(t, "drop1", drop1uuid, 1)
		t.Run("t3drop complete", func(t *testing.T) {
			// drop3 migration should not block. It can run concurrently to t1, and does not conflict
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, drop3uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop3uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("t1drop blocked", func(t *testing.T) {
			// drop1 migration should block. It can run concurrently to t1, but conflicts on table name
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
		})
		t.Run("t4 postponed", func(t *testing.T) {
			// drop4 migration should postpone.
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop4uuid, schema.OnlineDDLStatusQueued)
			// Issue a complete and wait for successful completion. drop4 is non-conflicting and should be able to proceed
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, drop4uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, drop4uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop4uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("complete t1", func(t *testing.T) {
			// t1 should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("t1drop unblocked", func(t *testing.T) {
			// t1drop should now be unblocked!
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, drop1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t1Name, false)
		})
		t.Run("revert t1 drop", func(t *testing.T) {
			revertDrop3uuid := testRevertMigration(t, createRevertParams(drop1uuid, ddlStrategy+" -allow-concurrent", "vtgate", "", true))
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, revertDrop3uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, revertDrop3uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t1Name, true)
		})
	})
	t.Run("conflicting migration does not block other queued migrations", func(t *testing.T) {
		t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy, "vtgate", "", "", false)) // skip wait
		t.Run("trivial t1 migration", func(t *testing.T) {
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t1Name, true)
		})

		t1uuid = testRevertMigration(t, createRevertParams(t1uuid, ddlStrategy+" -postpone-completion", "vtgate", "", true))
		t.Run("expect t1 revert migration to run", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
		})
		drop1uuid := testOnlineDDLStatement(t, createParams(dropT1Statement, ddlStrategy+" -allow-concurrent", "vtgate", "", "", true)) // skip wait
		t.Run("t1drop blocked", func(t *testing.T) {
			time.Sleep(ensureStateNotChangedTime)
			// drop1 migration should block. It can run concurrently to t1, but conflicts on table name
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusReady)
		})
		t.Run("t3 ready to complete", func(t *testing.T) {
			waitForReadyToComplete(t, drop1uuid, true)
		})
		t.Run("t3drop complete", func(t *testing.T) {
			// drop3 migration should not block. It can run concurrently to t1, and does not conflict
			// even though t1drop is blocked! This is the heart of this test
			drop3uuid := testOnlineDDLStatement(t, createParams(dropT3Statement, ddlStrategy+" -allow-concurrent", "vtgate", "", "", false))
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop3uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("cancel drop1", func(t *testing.T) {
			// drop1 migration should block. It can run concurrently to t1, but conflicts on table name
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusReady)
			// let's cancel it
			onlineddl.CheckCancelMigration(t, &vtParams, shards, drop1uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, drop1uuid, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, drop1uuid, schema.OnlineDDLStatusCancelled)
		})
		t.Run("complete t1", func(t *testing.T) {
			// t1 should be still running!
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusRunning)
			// Issue a complete and wait for successful completion
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			// This part may take a while, because we depend on vreplication polling
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
	})

	t.Run("Idempotent submission, retry failed migration", func(t *testing.T) {
		uuid := "00000000_1111_2222_3333_444444444444"
		overrideVtctlParams = &cluster.ApplySchemaParams{DDLStrategy: ddlStrategy, UUIDs: uuid, MigrationContext: "idempotent:1111-2222-3333"}
		defer func() { overrideVtctlParams = nil }()
		// create a migration and cancel it. We don't let it complete. We want it in "failed" state
		t.Run("start and fail migration", func(t *testing.T) {
			executedUUID := testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" -postpone-completion", "vtctl", "", "", true)) // skip wait
			require.Equal(t, uuid, executedUUID)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			// let's cancel it
			onlineddl.CheckCancelMigration(t, &vtParams, shards, uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusCancelled)
		})

		// now, we submit the exact same migration again: same UUID, same migration context.
		t.Run("resubmit migration", func(t *testing.T) {
			executedUUID := testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy, "vtctl", "", "", true)) // skip wait
			require.Equal(t, uuid, executedUUID)

			// expect it to complete
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)

			rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				retries := row.AsInt64("retries", 0)
				assert.Greater(t, retries, int64(0))

				cutOverThresholdSeconds := row.AsInt64("cutover_threshold_seconds", 0)
				// No explicit cut-over threshold given. Expect the default 10s
				assert.EqualValues(t, 10, cutOverThresholdSeconds)
			}
		})
	})

	t.Run("Idempotent submission, retry failed migration in singleton context", func(t *testing.T) {
		uuid := "00000000_1111_3333_3333_444444444444"
		ddlStrategy := ddlStrategy + " --singleton-context"
		overrideVtctlParams = &cluster.ApplySchemaParams{DDLStrategy: ddlStrategy, UUIDs: uuid, MigrationContext: "idempotent:1111-3333-3333"}
		defer func() { overrideVtctlParams = nil }()
		// create a migration and cancel it. We don't let it complete. We want it in "failed" state
		t.Run("start and fail migration", func(t *testing.T) {
			executedUUID := testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion", "vtctl", "", "", true)) // skip wait
			require.Equal(t, uuid, executedUUID)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			// let's cancel it
			onlineddl.CheckCancelMigration(t, &vtParams, shards, uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusCancelled)
		})

		// now, we submit the exact same migratoin again: same UUID, same migration context.
		t.Run("resubmit migration", func(t *testing.T) {
			executedUUID := testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy, "vtctl", "", "", true)) // skip wait
			require.Equal(t, uuid, executedUUID)

			t.Run("set low cut-over threshold", func(t *testing.T) {
				onlineddl.CheckSetMigrationCutOverThreshold(t, &vtParams, shards, uuid, 2*time.Second, "cut-over min value")
			})
			t.Run("set high cut-over threshold", func(t *testing.T) {
				onlineddl.CheckSetMigrationCutOverThreshold(t, &vtParams, shards, uuid, 2000*time.Second, "cut-over max value")
			})

			// expect it to complete
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)

			rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				retries := row.AsInt64("retries", 0)
				assert.Greater(t, retries, int64(0))

				cutOverThresholdSeconds := row.AsInt64("cutover_threshold_seconds", 0)
				// Teh default remains unchanged.
				assert.EqualValues(t, 10, cutOverThresholdSeconds)
			}
		})
	})

	readCleanupsTimetamps := func(t *testing.T, migrationsLike string) (rows int64, cleanedUp int64, needCleanup int64, artifacts []string) {
		rs := onlineddl.ReadMigrations(t, &vtParams, migrationsLike)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			rows++
			if row["cleanup_timestamp"].IsNull() {
				needCleanup++
			} else {
				cleanedUp++
			}
			migrationArtifacts := textutil.SplitDelimitedList(row.AsString("artifacts", ""))
			artifacts = append(artifacts, migrationArtifacts...)
		}
		return
	}

	t.Run("Cleanup artifacts", func(t *testing.T) {
		// Create a migration with a low --retain-artifacts value.
		// We will cancel the migration and expect the artifact to be cleaned.
		t.Run("start migration", func(t *testing.T) {
			t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion --retain-artifacts=1s", "vtctl", "", "", true)) // skip wait
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
		})
		t.Run("wait for ready_to_complete", func(t *testing.T) {
			waitForReadyToComplete(t, t1uuid, true)
		})
		var artifacts []string
		t.Run("validate artifact exists", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			row := rs.Named().Row()
			require.NotNil(t, row)

			artifacts = textutil.SplitDelimitedList(row.AsString("artifacts", ""))
			assert.NotEmpty(t, artifacts)
			assert.Equal(t, 1, len(artifacts))
			checkTable(t, artifacts[0], true)

			retainArtifactsSeconds := row.AsInt64("retain_artifacts_seconds", 0)
			assert.Equal(t, int64(1), retainArtifactsSeconds) // due to --retain-artifacts=1s
		})
		t.Run("cancel migration", func(t *testing.T) {
			onlineddl.CheckCancelMigration(t, &vtParams, shards, t1uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusCancelled)
		})
		t.Run("wait for cleanup", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), normalWaitTime)
			defer cancel()

			for {
				rows, cleanedUp, needCleanup, _ := readCleanupsTimetamps(t, t1uuid)
				assert.EqualValues(t, 1, rows)
				if cleanedUp == 1 {
					// This is what we've been waiting for
					break
				}
				assert.EqualValues(t, 0, cleanedUp)
				assert.EqualValues(t, 1, needCleanup)
				select {
				case <-ctx.Done():
					assert.Fail(t, "timeout waiting for cleanup")
					return
				case <-time.After(time.Second):
				}
			}
		})
		t.Run("validate artifact does not exist", func(t *testing.T) {
			checkTable(t, artifacts[0], false)
		})
	})

	t.Run("cleanup artifacts with CLEANUP ALL", func(t *testing.T) {
		// First, cleanup any existing migrations. We don't have an exact track of how many we've had so far.
		t.Run("initial cleanup all", func(t *testing.T) {
			t.Run("validate migrations exist that need cleanup", func(t *testing.T) {
				_, _, needCleanup, _ := readCleanupsTimetamps(t, "%")
				assert.Greater(t, needCleanup, int64(1))
			})
			t.Run("issue cleanup all", func(t *testing.T) {
				cleanedUp := onlineddl.CheckCleanupAllMigrations(t, &vtParams, -1)
				t.Logf("marked %d migrations for cleanup", cleanedUp)
			})
			t.Run("wait for all migrations cleanup", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), extendedWaitTime)
				defer cancel()

				for {
					rows, cleanedUp, needCleanup, artifacts := readCleanupsTimetamps(t, "%")
					if needCleanup == 0 {
						// This is what we've been waiting for
						assert.NotZero(t, rows)
						assert.Equal(t, rows, cleanedUp)
						assert.Empty(t, artifacts)
						t.Logf("rows needing cleanup: %v", needCleanup)
						return
					}
					select {
					case <-ctx.Done():
						assert.Fail(t, "timeout waiting for cleanup", "rows needing cleanup: %v. artifacts: %v", needCleanup, artifacts)
						return
					case <-time.After(time.Second):
					}
					t.Logf("rows needing cleanup: %v. artifacts: %v", needCleanup, artifacts)
				}
			})
		})
		// Create a migration with a low --retain-artifacts value.
		// We will cancel the migration and expect the artifact to be cleaned.
		t.Run("start migration", func(t *testing.T) {
			// Intentionally set `--retain-artifacts=1h` which is a long time. Then we will issue
			// `ALTER VITESS_MIGRATION CLEANUP ALL` and expect the artifact to be cleaned.
			t1uuid = testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, ddlStrategy+" --postpone-completion --retain-artifacts=1h", "vtctl", "", "", true)) // skip wait
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
		})
		t.Run("wait for ready_to_complete", func(t *testing.T) {
			waitForReadyToComplete(t, t1uuid, true)
		})
		var artifacts []string
		t.Run("validate artifact exists", func(t *testing.T) {
			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			row := rs.Named().Row()
			require.NotNil(t, row)

			artifacts = textutil.SplitDelimitedList(row.AsString("artifacts", ""))
			require.Len(t, artifacts, 1)
			checkTable(t, artifacts[0], true)

			retainArtifactsSeconds := row.AsInt64("retain_artifacts_seconds", 0)
			assert.EqualValues(t, 3600, retainArtifactsSeconds) // due to --retain-artifacts=1h
		})
		t.Run("check needs cleanup", func(t *testing.T) {
			_, _, needCleanup, _ := readCleanupsTimetamps(t, "%")
			assert.EqualValues(t, 1, needCleanup)
		})
		t.Run("complete migration", func(t *testing.T) {
			onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("cleanup all", func(t *testing.T) {
			onlineddl.CheckCleanupAllMigrations(t, &vtParams, 1)
		})
		t.Run("wait for migration cleanup", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), extendedWaitTime)
			defer cancel()

			for {
				rows, cleanedUp, needCleanup, artifacts := readCleanupsTimetamps(t, "%")
				if needCleanup == 0 {
					// This is what we've been waiting for
					assert.NotZero(t, rows)
					assert.Equal(t, rows, cleanedUp)
					assert.Empty(t, artifacts)
					t.Logf("rows needing cleanup: %v", needCleanup)
					return
				}
				select {
				case <-ctx.Done():
					assert.Fail(t, "timeout waiting for cleanup", "rows needing cleanup: %v. artifacts: %v", needCleanup, artifacts)
					return
				case <-time.After(time.Second):
				}
				t.Logf("rows needing cleanup: %v. artifacts: %v", needCleanup, artifacts)
			}
		})

		t.Run("validate artifact does not exist", func(t *testing.T) {
			checkTable(t, artifacts[0], false)
		})
	})

	checkConstraintCapable, err := capableOf(capabilities.CheckConstraintsCapability) // 8.0.16 and above
	require.NoError(t, err)
	if checkConstraintCapable {
		// Constraints
		t.Run("CREATE TABLE with CHECK constraint", func(t *testing.T) {
			query := `create table with_constraint (id int primary key, check ((id >= 0)))`
			uuid := testOnlineDDLStatement(t, createParams(query, ddlStrategy, "vtgate", "chk_", "", false))
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
			t.Run("ensure constraint name is rewritten", func(t *testing.T) {
				// Since we did not provide a name for the CHECK constraint, MySQL will
				// name it `with_constraint_chk_1`. But we expect Online DDL to explicitly
				// modify the constraint name, specifically to get rid of the <table-name> prefix,
				// so that we don't get into https://bugs.mysql.com/bug.php?id=107772 situation.
				createStatement := getCreateTableStatement(t, primaryTablet, "with_constraint")
				assert.NotContains(t, createStatement, "with_constraint_chk")
			})
		})
	}

	// INSTANT DDL
	instantDDLCapable, err := capableOf(capabilities.InstantAddLastColumnFlavorCapability)
	require.NoError(t, err)
	if instantDDLCapable {
		t.Run("INSTANT DDL: postpone-completion", func(t *testing.T) {
			t1uuid := testOnlineDDLStatement(t, createParams(instantAlterT1Statement, ddlStrategy+" --prefer-instant-ddl --postpone-completion", "vtgate", "", "", true))

			t.Run("expect t1 queued", func(t *testing.T) {
				// we want to validate that the migration remains queued even after some time passes. It must not move beyond 'queued'
				time.Sleep(ensureStateNotChangedTime)
				onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
			})
			t.Run("complete t1", func(t *testing.T) {
				// Issue a complete and wait for successful completion
				onlineddl.CheckCompleteMigration(t, &vtParams, shards, t1uuid, true)
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			})
		})
	}
	// Failure scenarios
	t.Run("fail nonexistent", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterNonexistent, "vitess", "vtgate", "", "", false))

		status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)

		rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			message := row["message"].ToString()
			require.Contains(t, message, "errno 1146")
			require.False(t, row["message_timestamp"].IsNull())
		}
	})

	// 'mysql' strategy
	t.Run("mysql strategy", func(t *testing.T) {
		t.Run("declarative", func(t *testing.T) {
			t1uuid = testOnlineDDLStatement(t, createParams(createT1Statement, "mysql --declarative", "vtgate", "just-created", "", false))

			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
			checkTable(t, t1Name, true)
		})

		t.Run("fail postpone-completion", func(t *testing.T) {
			t1uuid := testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, "mysql --postpone-completion", "vtgate", "", "", true))

			// --postpone-completion not supported in mysql strategy
			time.Sleep(ensureStateNotChangedTime)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusFailed)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusFailed)
		})
		t.Run("trivial", func(t *testing.T) {
			t1uuid := testOnlineDDLStatement(t, createParams(trivialAlterT1Statement, "mysql", "vtgate", "", "", true))

			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)

			rs := onlineddl.ReadMigrations(t, &vtParams, t1uuid)
			require.NotNil(t, rs)
			for _, row := range rs.Named().Rows {
				artifacts := row.AsString("artifacts", "-")
				assert.Empty(t, artifacts)
			}
		})
		t.Run("instant", func(t *testing.T) {
			t1uuid := testOnlineDDLStatement(t, createParams(instantAlterT1Statement, "mysql", "vtgate", "", "", true))

			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
	})
	// in-order-completion
	t.Run("in-order-completion: multiple drops for nonexistent tables and views, sequential", func(t *testing.T) {
		u, err := schema.CreateOnlineDDLUUID()
		require.NoError(t, err)

		sqls := []string{
			fmt.Sprintf("drop table if exists t4_%s", u),
			fmt.Sprintf("drop view  if exists t1_%s", u),
			fmt.Sprintf("drop table if exists t2_%s", u),
			fmt.Sprintf("drop view  if exists t3_%s", u),
		}
		sql := strings.Join(sqls, ";")
		var vuuids []string
		t.Run("drop multiple tables and views, in-order-completion", func(t *testing.T) {
			uuidList := testOnlineDDLStatement(t, createParams(sql, ddlStrategy+" --in-order-completion", "vtctl", "", "", true)) // skip wait
			vuuids = strings.Split(uuidList, "\n")
			assert.Len(t, vuuids, 4)
			for _, uuid := range vuuids {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
			}
		})
		require.Len(t, vuuids, 4)
		for i := range vuuids {
			if i > 0 {
				testTableCompletionTimes(t, vuuids[i-1], vuuids[i])
				testTableCompletionAndStartTimes(t, vuuids[i-1], vuuids[i])
			}
		}
	})
	t.Run("in-order-completion: multiple drops for nonexistent tables and views", func(t *testing.T) {
		u, err := schema.CreateOnlineDDLUUID()
		require.NoError(t, err)

		sqls := []string{
			fmt.Sprintf("drop table if exists t4_%s", u),
			fmt.Sprintf("drop view  if exists t1_%s", u),
			fmt.Sprintf("drop table if exists t2_%s", u),
			fmt.Sprintf("drop view  if exists t3_%s", u),
		}
		sql := strings.Join(sqls, ";")
		var vuuids []string
		t.Run("drop multiple tables and views, in-order-completion", func(t *testing.T) {
			uuidList := testOnlineDDLStatement(t, createParams(sql, ddlStrategy+" --allow-concurrent --in-order-completion", "vtctl", "", "", true)) // skip wait
			vuuids = strings.Split(uuidList, "\n")
			assert.Len(t, vuuids, 4)
			for _, uuid := range vuuids {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
			}
		})
		require.Len(t, vuuids, 4)
		for i := range vuuids {
			if i > 0 {
				testTableCompletionTimes(t, vuuids[i-1], vuuids[i])
			}
		}
	})

	t.Run("in-order-completion: bail out on first error", func(t *testing.T) {
		u, err := schema.CreateOnlineDDLUUID()
		require.NoError(t, err)

		sqls := []string{
			fmt.Sprintf("drop table if exists t4_%s", u),
			fmt.Sprintf("drop view  if exists t1_%s", u),
			fmt.Sprintf("drop table t2_%s", u), // non existent
			fmt.Sprintf("drop view  if exists t3_%s", u),
		}
		sql := strings.Join(sqls, ";")
		var vuuids []string
		t.Run("apply schema", func(t *testing.T) {
			uuidList := testOnlineDDLStatement(t, createParams(sql, ddlStrategy+" --in-order-completion", "vtctl", "", "", true)) // skip wait
			vuuids = strings.Split(uuidList, "\n")
			assert.Len(t, vuuids, 4)
			for _, uuid := range vuuids[0:2] {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
			}
			{
				uuid := vuuids[2] // the failed one
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
			}
			{
				uuid := vuuids[3] // should consequently fail without even running
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusFailed)

				rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
				require.NotNil(t, rs)
				for _, row := range rs.Named().Rows {
					message := row["message"].ToString()
					require.Contains(t, message, vuuids[2]) // Indicating this migration failed due to vuuids[2] failure
					require.False(t, row["message_timestamp"].IsNull())
				}
			}
		})
		testTableCompletionTimes(t, vuuids[0], vuuids[1])
		testTableCompletionAndStartTimes(t, vuuids[0], vuuids[1])
		testTableCompletionAndStartTimes(t, vuuids[1], vuuids[2])
	})
	t.Run("in-order-completion concurrent: bail out on first error", func(t *testing.T) {
		sqls := []string{
			`alter table t1_test force`,
			`alter table t2_test force`,
		}
		sql := strings.Join(sqls, ";")
		var vuuids []string
		t.Run("apply schema", func(t *testing.T) {
			uuidList := testOnlineDDLStatement(t, createParams(sql, ddlStrategy+" --in-order-completion --postpone-completion --allow-concurrent", "vtctl", "", "", true)) // skip wait
			vuuids = strings.Split(uuidList, "\n")
			assert.Len(t, vuuids, 2)
			for _, uuid := range vuuids {
				waitForReadyToComplete(t, uuid, true)
			}
			t.Run("cancel 1st migration", func(t *testing.T) {
				onlineddl.CheckCancelMigration(t, &vtParams, shards, vuuids[0], true)
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, vuuids[0], normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, vuuids[0], schema.OnlineDDLStatusCancelled)
			})
			t.Run("expect 2nd migration to fail", func(t *testing.T) {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, vuuids[1], normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, vuuids[1], schema.OnlineDDLStatusFailed)
			})
		})
	})
	t.Run("in-order-completion: two new views, one depends on the other", func(t *testing.T) {
		u, err := schema.CreateOnlineDDLUUID()
		require.NoError(t, err)
		v2name := fmt.Sprintf("v2_%s", u)
		createv2 := fmt.Sprintf("create view %s as select id from t1_test", v2name)
		v1name := fmt.Sprintf("v1_%s", u)
		createv1 := fmt.Sprintf("create view %s as select id from %s", v1name, v2name)

		sql := fmt.Sprintf("%s; %s;", createv2, createv1)
		var vuuids []string
		t.Run("create two views, expect both complete", func(t *testing.T) {
			uuidList := testOnlineDDLStatement(t, createParams(sql, ddlStrategy+" --allow-concurrent --in-order-completion", "vtctl", "", "", true)) // skip wait
			vuuids = strings.Split(uuidList, "\n")
			assert.Len(t, vuuids, 2)
			for _, uuid := range vuuids {
				status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
				fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
				onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
			}
		})
		require.Len(t, vuuids, 2)
		testTableCompletionTimes(t, vuuids[0], vuuids[1])
	})
	t.Run("in-order-completion: new table column, new view depends on said column", func(t *testing.T) {
		// The VIEW creation can only succeed when the ALTER has completed and the table has the new column
		t1uuid = testOnlineDDLStatement(t, createParams(alterExtraColumn, ddlStrategy+" --allow-concurrent --postpone-completion --in-order-completion", "vtctl", "", "", true))                // skip wait
		v1uuid := testOnlineDDLStatement(t, createParams(createViewDependsOnExtraColumn, ddlStrategy+" --allow-concurrent --postpone-completion --in-order-completion", "vtctl", "", "", true)) // skip wait

		testAllowConcurrent(t, "t1", t1uuid, 1)
		testAllowConcurrent(t, "v1", v1uuid, 1)
		t.Run("expect table running, expect view ready", func(t *testing.T) {
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, v1uuid, normalWaitTime, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
			time.Sleep(ensureStateNotChangedTime)
			// nothing should change
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusRunning)
			onlineddl.WaitForMigrationStatus(t, &vtParams, shards, v1uuid, normalWaitTime, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady)
		})
		t.Run("complete both", func(t *testing.T) {
			onlineddl.CheckCompleteAllMigrations(t, &vtParams, len(shards)*2)
		})
		t.Run("expect table success", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, t1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, t1uuid, schema.OnlineDDLStatusComplete)
		})
		t.Run("expect view success", func(t *testing.T) {
			status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, v1uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
			fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, v1uuid, schema.OnlineDDLStatusComplete)
		})
		testTableCompletionTimes(t, t1uuid, v1uuid)
	})
}

func testSingleton(t *testing.T) {
	shards = clusterInstance.Keyspaces[0].Shards
	require.Equal(t, 1, len(shards))

	createParams := func(ddlStatement string, ddlStrategy string, executeStrategy string, migrationContext string, expectHint string, expectError string, skipWait bool) *testOnlineDDLStatementParams {
		return &testOnlineDDLStatementParams{
			ddlStatement:     ddlStatement,
			ddlStrategy:      ddlStrategy,
			executeStrategy:  executeStrategy,
			migrationContext: migrationContext,
			expectHint:       expectHint,
			expectError:      expectError,
			skipWait:         skipWait,
		}
	}

	createRevertParams := func(revertUUID string, ddlStrategy string, executeStrategy string, migrationContext string, expectError string, skipWait bool) *testRevertMigrationParams {
		return &testRevertMigrationParams{
			revertUUID:       revertUUID,
			executeStrategy:  executeStrategy,
			ddlStrategy:      ddlStrategy,
			migrationContext: migrationContext,
			expectError:      expectError,
			skipWait:         skipWait,
		}
	}

	var (
		tableName                         = `stress_test`
		onlineSingletonDDLStrategy        = "online --singleton"
		onlineSingletonContextDDLStrategy = "online --singleton-context"
		createStatement                   = `
		CREATE TABLE stress_test (
			id bigint(20) not null,
			rand_val varchar(32) null default '',
			hint_col varchar(64) not null default 'just-created',
			created_timestamp timestamp not null default current_timestamp,
			updates int unsigned not null default 0,
			PRIMARY KEY (id),
			key created_idx(created_timestamp),
			key updates_idx(updates)
		) ENGINE=InnoDB
	`
		alterTableThrottlingStatement = `
		ALTER TABLE stress_test DROP COLUMN created_timestamp
	`
		multiAlterTableThrottlingStatement = `
		ALTER TABLE stress_test ENGINE=InnoDB;
		ALTER TABLE stress_test ENGINE=InnoDB;
		ALTER TABLE stress_test ENGINE=InnoDB;
	`
		// A trivial statement which must succeed and does not change the schema
		alterTableTrivialStatement = `
		ALTER TABLE stress_test ENGINE=InnoDB
	`
		dropStatement = `
	DROP TABLE stress_test
`
		dropIfExistsStatement = `
DROP TABLE IF EXISTS stress_test
`
		dropNonexistentTableStatement = `
		DROP TABLE IF EXISTS t_non_existent
	`
		multiDropStatements = `DROP TABLE IF EXISTS t1; DROP TABLE IF EXISTS t2; DROP TABLE IF EXISTS t3;`
	)

	var uuids []string
	// init-cleanup
	t.Run("init DROP TABLE", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(dropIfExistsStatement, onlineSingletonDDLStrategy, "vtgate", "", "", "", false))
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, false)
	})

	// CREATE
	t.Run("CREATE TABLE", func(t *testing.T) {
		// The table does not exist
		uuid := testOnlineDDLStatement(t, createParams(createStatement, onlineSingletonDDLStrategy, "vtgate", "", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, true)
	})
	t.Run("revert CREATE TABLE", func(t *testing.T) {
		require.NotEmpty(t, uuids)
		// The table existed, so it will now be dropped (renamed)
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1], onlineSingletonDDLStrategy, "vtgate", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, false)
	})
	t.Run("revert revert CREATE TABLE", func(t *testing.T) {
		// Table was dropped (renamed) so it will now be restored
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1], onlineSingletonDDLStrategy, "vtgate", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, true)
	})

	var openEndedUUID string
	t.Run("open ended migration", func(t *testing.T) {
		openEndedUUID = testOnlineDDLStatement(t, createParams(alterTableThrottlingStatement, "vitess --singleton --postpone-completion", "vtgate", "", "hint_col", "", false))
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, openEndedUUID, schema.OnlineDDLStatusRunning)
	})
	t.Run("failed singleton migration, vtgate", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableThrottlingStatement, "vitess --singleton --postpone-completion", "vtgate", "", "hint_col", "rejected", true))
		assert.Empty(t, uuid)
	})
	t.Run("failed singleton migration, vtctl", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableThrottlingStatement, "vitess --singleton --postpone-completion", "vtctl", "", "hint_col", "rejected", true))
		assert.Empty(t, uuid)
	})
	t.Run("failed revert migration", func(t *testing.T) {
		uuid := testRevertMigration(t, createRevertParams(openEndedUUID, onlineSingletonDDLStrategy, "vtgate", "", "rejected", true))
		assert.Empty(t, uuid)
	})
	t.Run("terminate throttled migration", func(t *testing.T) {
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, openEndedUUID, schema.OnlineDDLStatusRunning)
		onlineddl.CheckCancelMigration(t, &vtParams, shards, openEndedUUID, true)
		status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, openEndedUUID, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, openEndedUUID, schema.OnlineDDLStatusCancelled)
	})
	t.Run("successful alter, vtctl", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableTrivialStatement, "vitess --singleton", "vtctl", "", "hint_col", "", false))
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckCancelMigration(t, &vtParams, shards, uuid, false)
		onlineddl.CheckRetryMigration(t, &vtParams, shards, uuid, false)
	})
	t.Run("successful alter, vtgate", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableTrivialStatement, "vitess --singleton", "vtgate", "", "hint_col", "", false))
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckCancelMigration(t, &vtParams, shards, uuid, false)
		onlineddl.CheckRetryMigration(t, &vtParams, shards, uuid, false)
	})

	t.Run("successful online alter, vtgate", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableTrivialStatement, onlineSingletonDDLStrategy, "vtgate", "", "hint_col", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckCancelMigration(t, &vtParams, shards, uuid, false)
		onlineddl.CheckRetryMigration(t, &vtParams, shards, uuid, false)
		checkTable(t, tableName, true)
	})
	t.Run("revert ALTER TABLE, vttablet", func(t *testing.T) {
		// The table existed, so it will now be dropped (renamed)
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1], onlineSingletonDDLStrategy, "vtctl", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, true)
	})

	// singleton-table
	t.Run("fail singleton-table same table single submission", func(t *testing.T) {
		_ = testOnlineDDLStatement(t, createParams(multiAlterTableThrottlingStatement, "vitess --singleton-table", "vtctl", "", "hint_col", "singleton-table migration rejected", false))
		// The first of those migrations will make it, the other two will be rejected
		onlineddl.CheckCancelAllMigrations(t, &vtParams, 1)
	})
	t.Run("fail singleton-table same table multi submission", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableThrottlingStatement, "vitess --singleton-table --postpone-completion", "vtctl", "", "hint_col", "", false))
		uuids = append(uuids, uuid)
		onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusRunning)

		_ = testOnlineDDLStatement(t, createParams(alterTableThrottlingStatement, "vitess --singleton-table --postpone-completion", "vtctl", "", "hint_col", "singleton-table migration rejected", false))
		onlineddl.CheckCancelAllMigrations(t, &vtParams, 1)
	})

	var throttledUUIDs []string
	// singleton-context
	t.Run("postponed migrations, singleton-context", func(t *testing.T) {
		uuidList := testOnlineDDLStatement(t, createParams(multiAlterTableThrottlingStatement, "vitess --singleton-context --postpone-completion", "vtctl", "", "hint_col", "", false))
		throttledUUIDs = strings.Split(uuidList, "\n")
		assert.Equal(t, 3, len(throttledUUIDs))
		for _, uuid := range throttledUUIDs {
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady, schema.OnlineDDLStatusRunning)
		}
	})
	t.Run("failed migrations, singleton-context", func(t *testing.T) {
		_ = testOnlineDDLStatement(t, createParams(multiAlterTableThrottlingStatement, "vitess --singleton-context --postpone-completion", "vtctl", "", "hint_col", "rejected", false))
	})
	t.Run("terminate throttled migrations", func(t *testing.T) {
		for _, uuid := range throttledUUIDs {
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusQueued, schema.OnlineDDLStatusReady, schema.OnlineDDLStatusRunning)
			onlineddl.CheckCancelMigration(t, &vtParams, shards, uuid, true)
		}
		time.Sleep(2 * time.Second)
		for _, uuid := range throttledUUIDs {
			uuid = strings.TrimSpace(uuid)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
		}
	})

	t.Run("successful multiple statement, singleton-context, vtctl", func(t *testing.T) {
		uuidList := testOnlineDDLStatement(t, createParams(multiDropStatements, onlineSingletonContextDDLStrategy, "vtctl", "", "", "", false))
		uuidSlice := strings.Split(uuidList, "\n")
		assert.Equal(t, 3, len(uuidSlice))
		for _, uuid := range uuidSlice {
			uuid = strings.TrimSpace(uuid)
			onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		}
	})

	// DROP

	t.Run("online DROP TABLE", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(dropStatement, onlineSingletonDDLStrategy, "vtgate", "", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, false)
	})
	t.Run("revert DROP TABLE", func(t *testing.T) {
		// This will recreate the table (well, actually, rename it back into place)
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1], onlineSingletonDDLStrategy, "vttablet", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, true)
	})

	t.Run("fail concurrent singleton, vtgate", func(t *testing.T) {
		uuid := testOnlineDDLStatement(t, createParams(alterTableTrivialStatement, "vitess --postpone-completion --singleton", "vtgate", "", "hint_col", "", true))
		uuids = append(uuids, uuid)
		_ = testOnlineDDLStatement(t, createParams(dropNonexistentTableStatement, "vitess --singleton", "vtgate", "", "hint_col", "rejected", true))
		onlineddl.CheckCompleteAllMigrations(t, &vtParams, len(shards))
		status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
	})
	t.Run("fail concurrent singleton-context with revert", func(t *testing.T) {
		revertUUID := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1], "vitess --allow-concurrent --postpone-completion --singleton-context", "vtctl", "rev:ctx", "", false))
		onlineddl.WaitForMigrationStatus(t, &vtParams, shards, revertUUID, normalWaitTime, schema.OnlineDDLStatusRunning)
		// revert is running
		_ = testOnlineDDLStatement(t, createParams(dropNonexistentTableStatement, "vitess --allow-concurrent --singleton-context", "vtctl", "migrate:ctx", "", "rejected", true))
		onlineddl.CheckCancelMigration(t, &vtParams, shards, revertUUID, true)
		status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, revertUUID, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, revertUUID, schema.OnlineDDLStatusCancelled)
	})
	t.Run("success concurrent singleton-context with no-context revert", func(t *testing.T) {
		revertUUID := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1], "vitess --allow-concurrent --postpone-completion", "vtctl", "rev:ctx", "", false))
		onlineddl.WaitForMigrationStatus(t, &vtParams, shards, revertUUID, normalWaitTime, schema.OnlineDDLStatusRunning)
		// revert is running but has no --singleton-context. Our next migration should be able to run.
		uuid := testOnlineDDLStatement(t, createParams(dropNonexistentTableStatement, "vitess --allow-concurrent --singleton-context", "vtctl", "migrate:ctx", "", "", false))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckCancelMigration(t, &vtParams, shards, revertUUID, true)
		status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, revertUUID, normalWaitTime, schema.OnlineDDLStatusFailed, schema.OnlineDDLStatusCancelled)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, revertUUID, schema.OnlineDDLStatusCancelled)
	})
}
func testDeclarative(t *testing.T) {
	shards = clusterInstance.Keyspaces[0].Shards
	require.Equal(t, 1, len(shards))

	var (
		tableName         = `stress_test`
		viewBaseTableName = `view_base_table_test`
		viewName          = `view_test`
		migrationContext  = "1111-2222-3333"
		createStatement1  = `
			CREATE TABLE stress_test (
				id bigint(20) not null,
				rand_val varchar(32) null default '',
				hint_col varchar(64) not null default 'create1',
				created_timestamp timestamp not null default current_timestamp,
				updates int unsigned not null default 0,
				PRIMARY KEY (id),
				key created_idx(created_timestamp),
				key updates_idx(updates)
			) ENGINE=InnoDB
		`
		createStatement2 = `
			CREATE TABLE stress_test (
				id bigint(20) not null,
				rand_val varchar(32) null default '',
				hint_col varchar(64) not null default 'create2',
				created_timestamp timestamp not null default current_timestamp,
				updates int unsigned not null default 0,
				PRIMARY KEY (id),
				key created_idx(created_timestamp),
				key updates_idx(updates)
			) ENGINE=InnoDB
		`
		createIfNotExistsStatement = `
			CREATE TABLE IF NOT EXISTS stress_test (
				id bigint(20) not null,
				PRIMARY KEY (id)
			) ENGINE=InnoDB
		`
		createStatementZeroDate = `
			CREATE TABLE zerodate_test (
				id bigint(20) not null,
				hint_col varchar(64) not null default 'create_with_zero',
				zero_datetime datetime NOT NULL DEFAULT '0000-00-00 00:00:00',
				PRIMARY KEY (id)
			) ENGINE=InnoDB
		`
		createStatementZeroDate2 = `
			CREATE TABLE zerodate_test (
				id bigint(20) not null,
				i int not null default 0,
				hint_col varchar(64) not null default 'create_with_zero2',
				zero_datetime datetime NOT NULL DEFAULT '0000-00-00 00:00:00',
				zero_datetime2 datetime NOT NULL DEFAULT '0000-00-00 00:00:00',
				PRIMARY KEY (id)
			) ENGINE=InnoDB
		`
		dropZeroDateStatement = `
			DROP TABLE zerodate_test
		`
		dropStatement = `
			DROP TABLE stress_test
		`
		dropIfExistsStatement = `
			DROP TABLE IF EXISTS stress_test
		`
		alterStatement = `
			ALTER TABLE stress_test modify hint_col varchar(64) not null default 'this-should-fail'
		`
		trivialAlterStatement = `
			ALTER TABLE stress_test ENGINE=InnoDB
		`
		dropViewBaseTableStatement = `
			DROP TABLE IF EXISTS view_base_table_test
		`
		createViewBaseTableStatement = `
			CREATE TABLE view_base_table_test (id INT PRIMARY KEY)
		`
		createViewStatement1 = `
			CREATE VIEW view_test AS SELECT 'success_create1' AS msg FROM view_base_table_test
		`
		createViewStatement2 = `
			CREATE VIEW view_test AS SELECT 'success_create2' AS msg FROM view_base_table_test
		`
		createOrReplaceViewStatement = `
			CREATE OR REPLACE VIEW view_test AS SELECT 'success_replace' AS msg FROM view_base_table_test
		`
		alterViewStatement = `
			ALTER VIEW view_test AS SELECT 'success_alter' AS msg FROM view_base_table_test
		`
		dropViewStatement = `
			DROP VIEW view_test
		`
		dropViewIfExistsStatement = `
			DROP VIEW IF EXISTS view_test
		`
		insertRowStatement = `
			INSERT IGNORE INTO stress_test (id, rand_val) VALUES (%d, left(md5(rand()), 8))
		`
		updateRowStatement = `
			UPDATE stress_test SET updates=updates+1 WHERE id=%d
		`
		deleteRowStatement = `
			DELETE FROM stress_test WHERE id=%d AND updates=1
		`
		// We use CAST(SUM(updates) AS SIGNED) because SUM() returns a DECIMAL datatype, and we want to read a SIGNED INTEGER type
		selectCountRowsStatement = `
			SELECT COUNT(*) AS num_rows, CAST(SUM(updates) AS SIGNED) AS sum_updates FROM stress_test
		`
		truncateStatement = `
			TRUNCATE TABLE stress_test
		`
		writeMetrics WriteMetrics
		maxTableRows = 4096
	)

	declarativeStrategy := "online -declarative"
	var uuids []string

	generateInsert := func(t *testing.T, conn *mysql.Conn) error {
		id := rand.Int32N(int32(maxTableRows))
		query := fmt.Sprintf(insertRowStatement, id)
		qr, err := conn.ExecuteFetch(query, 1000, true)

		func() {
			writeMetrics.mu.Lock()
			defer writeMetrics.mu.Unlock()

			writeMetrics.insertsAttempts++
			if err != nil {
				writeMetrics.insertsFailures++
				return
			}
			assert.Less(t, qr.RowsAffected, uint64(2))
			if qr.RowsAffected == 0 {
				writeMetrics.insertsNoops++
				return
			}
			writeMetrics.inserts++
		}()
		return err
	}

	generateUpdate := func(t *testing.T, conn *mysql.Conn) error {
		id := rand.Int32N(int32(maxTableRows))
		query := fmt.Sprintf(updateRowStatement, id)
		qr, err := conn.ExecuteFetch(query, 1000, true)

		func() {
			writeMetrics.mu.Lock()
			defer writeMetrics.mu.Unlock()

			writeMetrics.updatesAttempts++
			if err != nil {
				writeMetrics.updatesFailures++
				return
			}
			assert.Less(t, qr.RowsAffected, uint64(2))
			if qr.RowsAffected == 0 {
				writeMetrics.updatesNoops++
				return
			}
			writeMetrics.updates++
		}()
		return err
	}

	generateDelete := func(t *testing.T, conn *mysql.Conn) error {
		id := rand.Int32N(int32(maxTableRows))
		query := fmt.Sprintf(deleteRowStatement, id)
		qr, err := conn.ExecuteFetch(query, 1000, true)

		func() {
			writeMetrics.mu.Lock()
			defer writeMetrics.mu.Unlock()

			writeMetrics.deletesAttempts++
			if err != nil {
				writeMetrics.deletesFailures++
				return
			}
			assert.Less(t, qr.RowsAffected, uint64(2))
			if qr.RowsAffected == 0 {
				writeMetrics.deletesNoops++
				return
			}
			writeMetrics.deletes++
		}()
		return err
	}

	initTable := func(t *testing.T) {
		log.Infof("initTable begin")
		defer log.Infof("initTable complete")

		ctx := context.Background()
		conn, err := mysql.Connect(ctx, &vtParams)
		require.Nil(t, err)
		defer conn.Close()

		writeMetrics.Clear()
		_, err = conn.ExecuteFetch(truncateStatement, 1000, true)
		require.Nil(t, err)

		for i := 0; i < maxTableRows/2; i++ {
			generateInsert(t, conn)
		}
		for i := 0; i < maxTableRows/4; i++ {
			generateUpdate(t, conn)
		}
		for i := 0; i < maxTableRows/4; i++ {
			generateDelete(t, conn)
		}
	}

	testSelectTableMetrics := func(t *testing.T) {
		writeMetrics.mu.Lock()
		defer writeMetrics.mu.Unlock()

		log.Infof("%s", writeMetrics.String())

		ctx := context.Background()
		conn, err := mysql.Connect(ctx, &vtParams)
		require.Nil(t, err)
		defer conn.Close()

		rs, err := conn.ExecuteFetch(selectCountRowsStatement, 1000, true)
		require.Nil(t, err)

		row := rs.Named().Row()
		require.NotNil(t, row)
		log.Infof("testSelectTableMetrics, row: %v", row)
		numRows := row.AsInt64("num_rows", 0)
		sumUpdates := row.AsInt64("sum_updates", 0)

		assert.NotZero(t, numRows)
		assert.NotZero(t, sumUpdates)
		assert.NotZero(t, writeMetrics.inserts)
		assert.NotZero(t, writeMetrics.deletes)
		assert.NotZero(t, writeMetrics.updates)
		assert.Equal(t, writeMetrics.inserts-writeMetrics.deletes, numRows)
		assert.Equal(t, writeMetrics.updates-writeMetrics.deletes, sumUpdates) // because we DELETE WHERE updates=1
	}

	testOnlineDDL := func(t *testing.T, alterStatement string, ddlStrategy string, executeStrategy string, expectHint string, expectError string) (uuid string) {
		params := &testOnlineDDLStatementParams{
			ddlStatement:    alterStatement,
			ddlStrategy:     ddlStrategy,
			executeStrategy: executeStrategy,
			expectHint:      expectHint,
			expectError:     expectError,
		}
		if executeStrategy != "vtgate" {
			params.migrationContext = migrationContext
		}
		return testOnlineDDLStatement(t, params)
	}
	createRevertParams := func(revertUUID string) *testRevertMigrationParams {
		return &testRevertMigrationParams{
			revertUUID:      revertUUID,
			executeStrategy: "vtctl",
			ddlStrategy:     string(schema.DDLStrategyOnline),
		}
	}

	// init-cleaup
	t.Run("init: drop table", func(t *testing.T) {
		// IF EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, dropIfExistsStatement, "online", "vtgate", "", "")
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
	})
	t.Run("init: drop view base table", func(t *testing.T) {
		// IF EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, dropViewBaseTableStatement, "online", "vtgate", "", "")
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
	})

	// VIEWS
	t.Run("create base table for view", func(t *testing.T) {
		uuid := testOnlineDDL(t, createViewBaseTableStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, viewBaseTableName, true)
	})
	// CREATE VIEW 1
	t.Run("declarative CREATE VIEW where table does not exist", func(t *testing.T) {
		// The table does not exist
		uuid := testOnlineDDL(t, createViewStatement1, declarativeStrategy, "vtgate", "success_create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, viewName, true)
	})
	// CREATE VIEW 1 again, noop
	t.Run("declarative CREATE VIEW with no changes where view exists", func(t *testing.T) {
		// The exists with exact same schema
		uuid := testOnlineDDL(t, createViewStatement1, declarativeStrategy, "vtgate", "success_create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, false)
		checkTable(t, viewName, true)
	})
	t.Run("revert CREATE VIEW expecting noop", func(t *testing.T) {
		// Reverting a noop changes nothing
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, viewName, "success_create1")
		checkTable(t, viewName, true)
	})
	// CREATE OR REPLACE VIEW
	t.Run("CREATE OR REPLACE VIEW expecting failure", func(t *testing.T) {
		// IF NOT EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, createOrReplaceViewStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
		checkMigratedTable(t, viewName, "success_create1")
		checkTable(t, viewName, true)
	})
	t.Run("ALTER VIEW expecting failure", func(t *testing.T) {
		// IF NOT EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, alterViewStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
		checkMigratedTable(t, viewName, "success_create1")
		checkTable(t, viewName, true)
	})
	t.Run("DROP VIEW IF EXISTS expecting failure", func(t *testing.T) {
		// IF NOT EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, dropViewIfExistsStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
		checkMigratedTable(t, viewName, "success_create1")
		checkTable(t, viewName, true)
	})
	t.Run("declarative DROP VIEW", func(t *testing.T) {
		uuid := testOnlineDDL(t, dropViewStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, viewName, false)
	})
	// View dropped. Let's start afresh.

	// CREATE VIEW1
	t.Run("declarative CREATE VIEW where view does not exist", func(t *testing.T) {
		// The table does not exist
		uuid := testOnlineDDL(t, createViewStatement1, declarativeStrategy, "vtgate", "success_create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, viewName, true)
	})
	// CREATE VIEW2: Change view
	t.Run("declarative CREATE VIEW with changes where view exists", func(t *testing.T) {
		// The table exists with different schema
		uuid := testOnlineDDL(t, createViewStatement2, declarativeStrategy, "vtgate", "success_create2", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, viewName, true)
	})
	t.Run("revert CREATE VIEW expecting previous schema", func(t *testing.T) {
		// Reverting back to 1st version
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, viewName, "success_create1")
		checkTable(t, viewName, true)
	})
	t.Run("declarative DROP VIEW", func(t *testing.T) {
		// Table exists
		uuid := testOnlineDDL(t, dropViewStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, viewName, false)
	})
	t.Run("revert DROP VIEW", func(t *testing.T) {
		// This will recreate the table (well, actually, rename it back into place)
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, viewName, true)
		checkMigratedTable(t, viewName, "success_create1")
	})
	t.Run("revert revert DROP VIEW", func(t *testing.T) {
		// This will reapply DROP VIEW
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, viewName, false)
	})
	t.Run("declarative DROP VIEW where view does not exist", func(t *testing.T) {
		uuid := testOnlineDDL(t, dropViewStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, false)
		checkTable(t, viewName, false)
	})
	t.Run("revert DROP VIEW where view did not exist", func(t *testing.T) {
		// Table will not be recreated because it didn't exist during the previous DROP VIEW
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, viewName, false)
	})
	// View dropped. Let's start afresh.

	// TABLES

	// CREATE1
	t.Run("declarative CREATE TABLE where table does not exist", func(t *testing.T) {
		// The table does not exist
		uuid := testOnlineDDL(t, createStatement1, declarativeStrategy, "vtgate", "create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, true)
		initTable(t)
		testSelectTableMetrics(t)
	})
	// CREATE1 again, noop
	t.Run("declarative CREATE TABLE with no changes where table exists", func(t *testing.T) {
		// The exists with exact same schema
		uuid := testOnlineDDL(t, createStatement1, declarativeStrategy, "vtgate", "create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, false)
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("revert CREATE TABLE expecting noop", func(t *testing.T) {
		// Reverting a noop changes nothing
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, tableName, "create1")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("declarative DROP TABLE", func(t *testing.T) {
		uuid := testOnlineDDL(t, dropStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, false)
	})
	// Table dropped. Let's start afresh.

	// CREATE1
	t.Run("declarative CREATE TABLE where table does not exist", func(t *testing.T) {
		// The table does not exist
		uuid := testOnlineDDL(t, createStatement1, declarativeStrategy, "vtgate", "create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, true)
		initTable(t)
		testSelectTableMetrics(t)
	})
	// CREATE2: Change schema
	t.Run("declarative CREATE TABLE with changes where table exists", func(t *testing.T) {
		// The table exists with different schema
		uuid := testOnlineDDL(t, createStatement2, declarativeStrategy, "vtgate", "create2", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("revert CREATE TABLE expecting previous schema", func(t *testing.T) {
		// Reverting back to 1st version
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, tableName, "create1")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("declarative DROP TABLE", func(t *testing.T) {
		// Table exists
		uuid := testOnlineDDL(t, dropStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, false)
	})
	t.Run("revert DROP TABLE", func(t *testing.T) {
		// This will recreate the table (well, actually, rename it back into place)
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, true)
		checkMigratedTable(t, tableName, "create1")
		testSelectTableMetrics(t)
	})
	t.Run("revert revert DROP TABLE", func(t *testing.T) {
		// This will reapply DROP TABLE
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, false)
	})
	t.Run("declarative DROP TABLE where table does not exist", func(t *testing.T) {
		uuid := testOnlineDDL(t, dropStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, false)
		checkTable(t, tableName, false)
	})
	t.Run("revert DROP TABLE where table did not exist", func(t *testing.T) {
		// Table will not be recreated because it didn't exist during the previous DROP TABLE
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkTable(t, tableName, false)
	})
	// Table dropped. Let's start afresh.

	// CREATE1
	t.Run("declarative CREATE TABLE where table does not exist", func(t *testing.T) {
		// The table does not exist
		uuid := testOnlineDDL(t, createStatement1, declarativeStrategy, "vtgate", "create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, true)
		initTable(t)
		testSelectTableMetrics(t)
	})
	// CREATE2
	t.Run("declarative CREATE TABLE with changes where table exists", func(t *testing.T) {
		// The table exists but with different schema
		uuid := testOnlineDDL(t, createStatement2, declarativeStrategy, "vtgate", "create2", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	// CREATE1 again
	t.Run("declarative CREATE TABLE again with changes where table exists", func(t *testing.T) {
		// The table exists but with different schema
		uuid := testOnlineDDL(t, createStatement1, declarativeStrategy, "vtgate", "create1", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		onlineddl.CheckMigrationArtifacts(t, &vtParams, shards, uuid, true)
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("revert CREATE TABLE expecting previous schema", func(t *testing.T) {
		// Reverting back to previous version
		uuid := testRevertMigration(t, createRevertParams(uuids[len(uuids)-1]))
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, tableName, "create2")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("ALTER TABLE expecting failure", func(t *testing.T) {
		// ALTER is not supported in -declarative
		uuid := testOnlineDDL(t, alterStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
		checkMigratedTable(t, tableName, "create2")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("CREATE TABLE IF NOT EXISTS expecting failure", func(t *testing.T) {
		// IF NOT EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, createIfNotExistsStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
		checkMigratedTable(t, tableName, "create2")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("DROP TABLE IF EXISTS expecting failure", func(t *testing.T) {
		// IF EXISTS is not supported in -declarative
		uuid := testOnlineDDL(t, dropIfExistsStatement, declarativeStrategy, "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
		checkMigratedTable(t, tableName, "create2")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("CREATE TABLE IF NOT EXISTS non-declarative is successful", func(t *testing.T) {
		// IF NOT EXISTS is supported in non-declarative mode. Just verifying that the statement itself is good,
		// so that the failure we tested for, above, actually tests the "declarative" logic, rather than some
		// unrelated error.
		uuid := testOnlineDDL(t, createIfNotExistsStatement, "online", "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		// the table existed, so we expect no changes in this non-declarative DDL
		checkMigratedTable(t, tableName, "create2")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("CREATE TABLE with zero date and --allow-zero-in-date is successful", func(t *testing.T) {
		uuid := testOnlineDDL(t, createStatementZeroDate, "online --allow-zero-in-date", "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, "zerodate_test", "create_with_zero")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("CREATE TABLE with zero date and --allow-zero-in-date is successful", func(t *testing.T) {
		uuid := testOnlineDDL(t, createStatementZeroDate, "online -declarative --allow-zero-in-date", "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, "zerodate_test", "create_with_zero")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})
	t.Run("CREATE TABLE with zero date and --allow-zero-in-date is successful", func(t *testing.T) {
		uuid := testOnlineDDL(t, createStatementZeroDate2, "online -declarative --allow-zero-in-date", "vtgate", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		checkMigratedTable(t, "zerodate_test", "create_with_zero2")
		checkTable(t, tableName, true)
		testSelectTableMetrics(t)
	})

	// ### The following tests are not strictly 'declarative' but are best served under this endtoend test

	// Test duplicate context/SQL
	t.Run("Trivial statement with request context is successful", func(t *testing.T) {
		uuid := testOnlineDDL(t, trivialAlterStatement, "online", "vtctl", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		// the table existed, so we expect no changes in this non-declarative DDL
		checkTable(t, tableName, true)

		rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			message := row["message"].ToString()
			require.NotContains(t, message, "duplicate DDL")
		}
	})
	t.Run("Duplicate trivial statement with request context is successful", func(t *testing.T) {
		uuid := testOnlineDDL(t, trivialAlterStatement, "online", "vtctl", "", "")
		uuids = append(uuids, uuid)
		onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
		// the table existed, so we expect no changes in this non-declarative DDL
		checkTable(t, tableName, true)

		rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
		require.NotNil(t, rs)
		for _, row := range rs.Named().Rows {
			message := row["message"].ToString()
			// Message suggests that the migration was identified as duplicate
			require.Contains(t, message, "duplicate DDL")
		}
	})
	// Piggyride this test suite, let's also test --allow-zero-in-date for 'direct' strategy
	t.Run("drop non_online", func(t *testing.T) {
		_ = testOnlineDDL(t, dropZeroDateStatement, "direct", "vtctl", "", "")
		checkTable(t, "zerodate_test", false)
	})
	t.Run("CREATE TABLE with zero date fails in 'direct' strategy", func(t *testing.T) {
		_ = testOnlineDDL(t, createStatementZeroDate, "direct", "vtctl", "", "Invalid default value for")
		checkTable(t, "zerodate_test", false)
	})
	t.Run("CREATE TABLE with zero date and --allow-zero-in-date succeeds in 'direct' strategy", func(t *testing.T) {
		_ = testOnlineDDL(t, createStatementZeroDate, "direct --allow-zero-in-date", "vtctl", "", "")
		checkTable(t, "zerodate_test", true)
	})
}

func testForeignKeys(t *testing.T) {

	var (
		createStatements = []string{
			`
			CREATE TABLE parent_table (
					id INT NOT NULL,
					parent_hint_col INT NOT NULL DEFAULT 0,
					PRIMARY KEY (id)
			)
		`,
			`
			CREATE TABLE child_table (
				id INT NOT NULL auto_increment,
				parent_id INT,
				child_hint_col INT NOT NULL DEFAULT 0,
				PRIMARY KEY (id),
				KEY parent_id_idx (parent_id),
				CONSTRAINT child_parent_fk FOREIGN KEY (parent_id) REFERENCES parent_table(id) ON DELETE CASCADE
			)
		`,
			`
			CREATE TABLE child_nofk_table (
				id INT NOT NULL auto_increment,
				parent_id INT,
				child_hint_col INT NOT NULL DEFAULT 0,
				PRIMARY KEY (id),
				KEY parent_id_idx (parent_id)
			)
		`,
		}
		insertStatements = []string{
			"insert into parent_table (id) values(43)",
			"insert into child_table (id, parent_id) values(1,43)",
			"insert into child_table (id, parent_id) values(2,43)",
			"insert into child_table (id, parent_id) values(3,43)",
			"insert into child_table (id, parent_id) values(4,43)",
		}
		ddlStrategy        = "online --allow-zero-in-date"
		ddlStrategyAllowFK = ddlStrategy + " --unsafe-allow-foreign-keys"
	)

	type testCase struct {
		name                      string
		sql                       string
		allowForeignKeys          bool
		expectHint                string
		expectCountUUIDs          int
		onlyIfFKOnlineDDLPossible bool
	}
	var testCases = []testCase{
		{
			name:             "modify parent, not allowed",
			sql:              "alter table parent_table engine=innodb",
			allowForeignKeys: false,
		},
		{
			name:             "modify child, not allowed",
			sql:              "alter table child_table engine=innodb",
			allowForeignKeys: false,
		},
		{
			name:             "add foreign key to child, not allowed",
			sql:              "alter table child_table add CONSTRAINT another_fk FOREIGN KEY (parent_id) REFERENCES parent_table(id) ON DELETE CASCADE",
			allowForeignKeys: false,
		},
		{
			name:             "add foreign key to table which wasn't a child before, not allowed",
			sql:              "alter table child_nofk_table add CONSTRAINT new_fk FOREIGN KEY (parent_id) REFERENCES parent_table(id) ON DELETE CASCADE",
			allowForeignKeys: false,
		},
		{
			// on vanilla MySQL, this migration ends with the child_table referencing the old, original table, and not to the new table now called parent_table.
			// This is a fundamental foreign key limitation, see https://vitess.io/blog/2021-06-15-online-ddl-why-no-fk/
			// However, this tests is still valid in the sense that it lets us modify the parent table in the first place.
			name:                      "modify parent, trivial",
			sql:                       "alter table parent_table engine=innodb",
			allowForeignKeys:          true,
			expectHint:                "parent_hint_col",
			onlyIfFKOnlineDDLPossible: true,
		},
		{
			// on vanilla MySQL, this migration ends with two tables, the original and the new child_table, both referencing parent_table. This has
			// the unwanted property of then limiting actions on the parent_table based on what rows exist or do not exist on the now stale old
			// child table.
			// This is a fundamental foreign key limitation, see https://vitess.io/blog/2021-06-15-online-ddl-why-no-fk/
			// However, this tests is still valid in the sense that it lets us modify the child table in the first place.
			// A valid use case: using FOREIGN_KEY_CHECKS=0  at all times.
			name:                      "modify child, trivial",
			sql:                       "alter table child_table engine=innodb",
			allowForeignKeys:          true,
			expectHint:                "REFERENCES `parent_table`",
			onlyIfFKOnlineDDLPossible: true,
		},
		{
			// on vanilla MySQL, this migration ends with two tables, the original and the new child_table, both referencing parent_table. This has
			// the unwanted property of then limiting actions on the parent_table based on what rows exist or do not exist on the now stale old
			// child table.
			// This is a fundamental foreign key limitation, see https://vitess.io/blog/2021-06-15-online-ddl-why-no-fk/
			// However, this tests is still valid in the sense that it lets us modify the child table in the first place.
			// A valid use case: using FOREIGN_KEY_CHECKS=0  at all times.
			name:                      "add foreign key to child",
			sql:                       "alter table child_table add CONSTRAINT another_fk FOREIGN KEY (parent_id) REFERENCES parent_table(id) ON DELETE CASCADE",
			allowForeignKeys:          true,
			expectHint:                "another_fk",
			onlyIfFKOnlineDDLPossible: true,
		},
		{
			name:             "add foreign key to table which wasn't a child before",
			sql:              "alter table child_nofk_table add CONSTRAINT new_fk FOREIGN KEY (parent_id) REFERENCES parent_table(id) ON DELETE CASCADE",
			allowForeignKeys: true,
			expectHint:       "new_fk",
		},
		{
			name:                      "drop foreign key from a child",
			sql:                       "alter table child_table DROP FOREIGN KEY <childTableConstraintName>", // See "getting child_table constraint name" test step below.
			allowForeignKeys:          true,
			expectHint:                "child_hint",
			onlyIfFKOnlineDDLPossible: true,
		},
		{
			name: "add two tables with cyclic fk relationship",
			sql: `
				create table t11 (id int primary key, i int, constraint f11 foreign key (i) references t12 (id));
				create table t12 (id int primary key, i int, constraint f12 foreign key (i) references t11 (id));
				`,
			allowForeignKeys: true,
			expectCountUUIDs: 2,
			expectHint:       "t11",
		},
	}

	fkOnlineDDLPossible := false
	t.Run("check 'rename_table_preserve_foreign_key' variable", func(t *testing.T) {
		// Online DDL is not possible on vanilla MySQL 8.0 for reasons described in https://vitess.io/blog/2021-06-15-online-ddl-why-no-fk/.
		// However, Online DDL is made possible in via these changes:
		// - https://github.com/planetscale/mysql-server/commit/bb777e3e86387571c044fb4a2beb4f8c60462ced
		// - https://github.com/planetscale/mysql-server/commit/c2f1344a6863518d749f2eb01a4c74ca08a5b889
		// as part of https://github.com/planetscale/mysql-server/releases/tag/8.0.34-ps3.
		// Said changes introduce a new global/session boolean variable named 'rename_table_preserve_foreign_key'. It defaults 'false'/0 for backwards compatibility.
		// When enabled, a `RENAME TABLE` to a FK parent "pins" the children's foreign keys to the table name rather than the table pointer. Which means after the RENAME,
		// the children will point to the newly instated table rather than the original, renamed table.
		// (Note: this applies to a particular type of RENAME where we swap tables, see the above blog post).
		// For FK children, the MySQL changes simply ignore any Vitess-internal table.
		//
		// In this stress test, we enable Online DDL if the variable 'rename_table_preserve_foreign_key' is present. The Online DDL mechanism will in turn
		// query for this variable, and manipulate it, when starting the migration and when cutting over.
		rs, err := primaryTablet.VttabletProcess.QueryTablet("show global variables like 'rename_table_preserve_foreign_key'", keyspaceName, false)
		require.NoError(t, err)
		fkOnlineDDLPossible = len(rs.Rows) > 0
		t.Logf("MySQL support for 'rename_table_preserve_foreign_key': %v", fkOnlineDDLPossible)
	})

	createParams := func(ddlStatement string, ddlStrategy string, executeStrategy string, expectHint string, expectError string, skipWait bool) *testOnlineDDLStatementParams {
		return &testOnlineDDLStatementParams{
			ddlStatement:    ddlStatement,
			ddlStrategy:     ddlStrategy,
			executeStrategy: executeStrategy,
			expectHint:      expectHint,
			expectError:     expectError,
			skipWait:        skipWait,
		}
	}

	testStatement := func(t *testing.T, sql string, ddlStrategy string, expectHint string, expectError bool) (uuid string) {
		errorHint := ""
		if expectError {
			errorHint = anyErrorIndicator
		}
		return testOnlineDDLStatement(t, createParams(sql, ddlStrategy, "vtctl", expectHint, errorHint, false))
	}
	for _, testcase := range testCases {
		if testcase.expectCountUUIDs == 0 {
			testcase.expectCountUUIDs = 1
		}
		t.Run(testcase.name, func(t *testing.T) {
			if testcase.onlyIfFKOnlineDDLPossible && !fkOnlineDDLPossible {
				t.Skipf("skipped because backing database does not support 'rename_table_preserve_foreign_key'")
				return
			}
			t.Run("create tables", func(t *testing.T) {
				for _, statement := range createStatements {
					t.Run(statement, func(t *testing.T) {
						uuid := testStatement(t, statement, ddlStrategyAllowFK, "", false)
						onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
					})
				}
			})
			t.Run("populate tables", func(t *testing.T) {
				for _, statement := range insertStatements {
					t.Run(statement, func(t *testing.T) {
						onlineddl.VtgateExecQuery(t, &vtParams, statement, "")
					})
				}
			})
			t.Run("getting child_table constraint name", func(t *testing.T) {
				// Due to how OnlineDDL works, the name of the foreign key constraint will not be the one we used in the CREATE TABLE statement.
				// There's a specific test where we drop said constraint. So speficially for that test (or any similar future tests), we need to dynamically
				// evaluate the constraint name.
				rs := onlineddl.VtgateExecQuery(t, &vtParams, "select CONSTRAINT_NAME from information_schema.REFERENTIAL_CONSTRAINTS where TABLE_NAME='child_table'", "")
				assert.Equal(t, 1, len(rs.Rows))
				row := rs.Named().Row()
				assert.NotNil(t, row)
				childTableConstraintName := row.AsString("CONSTRAINT_NAME", "")
				assert.NotEmpty(t, childTableConstraintName)
				testcase.sql = strings.ReplaceAll(testcase.sql, "<childTableConstraintName>", childTableConstraintName)
			})

			var uuid string
			t.Run("run migration", func(t *testing.T) {
				if testcase.allowForeignKeys {
					output := testStatement(t, testcase.sql, ddlStrategyAllowFK, testcase.expectHint, false)
					uuids := strings.Split(output, "\n")
					assert.Equal(t, testcase.expectCountUUIDs, len(uuids))
					uuid = uuids[0] // in case of multiple statements, we only check the first
					onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusComplete)
				} else {
					uuid = testStatement(t, testcase.sql, ddlStrategy, "", true)
					if uuid != "" {
						onlineddl.CheckMigrationStatus(t, &vtParams, shards, uuid, schema.OnlineDDLStatusFailed)
					}
				}
			})
			t.Run("cleanup", func(t *testing.T) {
				var artifacts []string
				if uuid != "" {
					rs := onlineddl.ReadMigrations(t, &vtParams, uuid)
					require.NotNil(t, rs)
					row := rs.Named().Row()
					require.NotNil(t, row)

					artifacts = textutil.SplitDelimitedList(row.AsString("artifacts", ""))
				}

				artifacts = append(artifacts, "child_table", "child_nofk_table", "parent_table", "t11", "t12")
				// brute force drop all tables. In MySQL 8.0 you can do a single `DROP TABLE ... <list of all tables>`
				// which auto-resovled order. But in 5.7 you can't.
				droppedTables := map[string]bool{}
				for range artifacts {
					for _, artifact := range artifacts {
						if droppedTables[artifact] {
							continue
						}
						statement := fmt.Sprintf("DROP TABLE IF EXISTS %s", artifact)
						_, err := clusterInstance.VtctldClientProcess.ApplySchemaWithOutput(keyspaceName, statement, cluster.ApplySchemaParams{DDLStrategy: "direct --unsafe-allow-foreign-keys"})
						if err == nil {
							droppedTables[artifact] = true
						}
					}
				}
				statement := fmt.Sprintf("DROP TABLE IF EXISTS %s", strings.Join(artifacts, ","))
				t.Run(statement, func(t *testing.T) {
					testStatement(t, statement, "direct", "", false)
				})
			})
		})
	}
}

// testOnlineDDLStatement runs an online DDL, ALTER statement
func testOnlineDDLStatement(t *testing.T, params *testOnlineDDLStatementParams) (uuid string) {
	strategySetting, err := schema.ParseDDLStrategy(params.ddlStrategy)
	require.NoError(t, err)

	tableName := parseTableName(t, params.ddlStatement)

	if params.executeStrategy == "vtgate" {
		require.Empty(t, params.migrationContext, "explicit migration context not supported in vtgate. Test via vtctl")
		result := onlineddl.VtgateExecDDL(t, &vtParams, params.ddlStrategy, params.ddlStatement, params.expectError)
		if result != nil {
			row := result.Named().Row()
			if row != nil {
				uuid = row.AsString("uuid", "")
			}
		}
	} else {
		vtctlParams := &cluster.ApplySchemaParams{DDLStrategy: params.ddlStrategy, MigrationContext: params.migrationContext}
		if overrideVtctlParams != nil {
			vtctlParams = overrideVtctlParams
		}
		output, err := clusterInstance.VtctldClientProcess.ApplySchemaWithOutput(keyspaceName, params.ddlStatement, *vtctlParams)
		switch params.expectError {
		case anyErrorIndicator:
			if err != nil {
				// fine. We got any error.
				t.Logf("expected any error, got this error: %v", err)
				return
			}
			uuid = output
		case "":
			assert.NoError(t, err)
			uuid = output
		default:
			assert.Error(t, err)
			assert.Contains(t, output, params.expectError)
		}
	}
	uuid = strings.TrimSpace(uuid)
	fmt.Println("# Generated UUID (for debug purposes):")
	fmt.Printf("<%s>\n", uuid)

	if !strategySetting.Strategy.IsDirect() && !params.skipWait && uuid != "" {
		status := onlineddl.WaitForMigrationStatus(t, &vtParams, shards, uuid, normalWaitTime, schema.OnlineDDLStatusComplete, schema.OnlineDDLStatusFailed)
		fmt.Printf("# Migration status (for debug purposes): <%s>\n", status)
	}

	if params.expectError == "" && params.expectHint != "" {
		checkMigratedTable(t, tableName, params.expectHint)
	}
	return uuid
}

// testRevertMigration reverts a given migration
func testRevertMigration(t *testing.T, params *testRevertMigrationParams) (uuid string) {
	revertQuery := fmt.Sprintf("revert vitess_migration '%s'", params.revertUUID)
	if params.executeStrategy == "vtgate" {
		require.Empty(t, params.migrationContext, "explicit migration context not supported in vtgate. Test via vtctl")
		result := onlineddl.VtgateExecDDL(t, &vtParams, params.ddlStrategy, revertQuery, params.expectError)
		if result != nil {
			row := result.Named().Row()
			if row != nil {
				uuid = row.AsString("uuid", "")
			}
		}
	} else {
		output, err := clusterInstance.VtctldClientProcess.ApplySchemaWithOutput(keyspaceName, revertQuery, cluster.ApplySchemaParams{DDLStrategy: params.ddlStrategy, MigrationContext: params.migrationContext})
		if params.expectError == "" {
			assert.NoError(t, err)
			uuid = output
		} else {
			assert.Error(t, err)
			assert.Contains(t, output, params.expectError)
		}
	}

	if params.expectError == "" {
		uuid = strings.TrimSpace(uuid)
		fmt.Println("# Generated UUID (for debug purposes):")
		fmt.Printf("<%s>\n", uuid)
	}
	if !params.skipWait {
		time.Sleep(time.Second * 20)
	}
	return uuid
}

// checkTable checks the number of tables in all shards
func checkTable(t *testing.T, showTableName string, expectExists bool) bool {
	expectCount := 0
	if expectExists {
		expectCount = 1
	}
	for i := range clusterInstance.Keyspaces[0].Shards {
		if !checkTablesCount(t, clusterInstance.Keyspaces[0].Shards[i].Vttablets[0], showTableName, expectCount) {
			return false
		}
	}
	return true
}

// checkTablesCount checks the number of tables in the given tablet
func checkTablesCount(t *testing.T, tablet *cluster.Vttablet, showTableName string, expectCount int) bool {
	query := fmt.Sprintf(`show tables like '%%%s%%';`, showTableName)
	queryResult, err := tablet.VttabletProcess.QueryTablet(query, keyspaceName, true)
	require.Nil(t, err)
	return assert.Equalf(t, expectCount, len(queryResult.Rows), "checkTablesCount cannot find table like '%%%s%%'", showTableName)
}

// checkMigratedTables checks the CREATE STATEMENT of a table after migration
func checkMigratedTable(t *testing.T, tableName, expectHint string) {
	for i := range clusterInstance.Keyspaces[0].Shards {
		createStatement := getCreateTableStatement(t, clusterInstance.Keyspaces[0].Shards[i].Vttablets[0], tableName)
		assert.Contains(t, createStatement, expectHint)
	}
}

// getCreateTableStatement returns the CREATE TABLE statement for a given table
func getCreateTableStatement(t *testing.T, tablet *cluster.Vttablet, tableName string) (statement string) {
	queryResult, err := tablet.VttabletProcess.QueryTablet(fmt.Sprintf("show create table %s;", tableName), keyspaceName, true)
	require.Nil(t, err)

	assert.Equal(t, len(queryResult.Rows), 1)
	assert.GreaterOrEqual(t, len(queryResult.Rows[0]), 2) // table name, create statement, and if it's a view then additional columns
	statement = queryResult.Rows[0][1].ToString()
	return statement
}

func runInTransaction(t *testing.T, ctx context.Context, tablet *cluster.Vttablet, query string, commitTransactionChan chan any, transactionErrorChan chan error) error {
	conn, err := tablet.VttabletProcess.TabletConn(keyspaceName, true)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecuteFetch("begin", 0, false)
	require.NoError(t, err)

	_, err = conn.ExecuteFetch(query, 10000, false)
	require.NoError(t, err)

	if commitTransactionChan != nil {
		// Wait for instruction to commit
		select {
		case <-commitTransactionChan:
			// good
		case <-ctx.Done():
			assert.Fail(t, ctx.Err().Error())
		}
	}

	_, err = conn.ExecuteFetch("commit", 0, false)
	if transactionErrorChan != nil {
		transactionErrorChan <- err
	}
	return err
}
