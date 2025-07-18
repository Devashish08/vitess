/*
Copyright 2024 The Vitess Authors.

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

package mysqlctl

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/ioutil"
	"vitess.io/vitess/go/mysql/fakesqldb"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/logutil"
	tabletmanagerdatapb "vitess.io/vitess/go/vt/proto/tabletmanagerdata"
)

func TestMySQLShellBackupBackupPreCheck(t *testing.T) {
	originalLocation := mysqlShellBackupLocation
	originalFlags := mysqlShellFlags
	defer func() {
		mysqlShellBackupLocation = originalLocation
		mysqlShellFlags = originalFlags
	}()

	engine := MySQLShellBackupEngine{}
	tests := []struct {
		name     string
		location string
		flags    string
		err      error
	}{
		{
			"empty flags",
			"",
			`{}`,
			ErrMySQLShellPreCheck,
		},
		{
			"only location",
			"/dev/null",
			"",
			ErrMySQLShellPreCheck,
		},
		{
			"only flags",
			"",
			"--js",
			ErrMySQLShellPreCheck,
		},
		{
			"both values present but without --js",
			"",
			"-h localhost",
			ErrMySQLShellPreCheck,
		},
		{
			"supported values",
			t.TempDir(),
			"--js -h localhost",
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			mysqlShellBackupLocation = tt.location
			mysqlShellFlags = tt.flags
			assert.ErrorIs(t, engine.backupPreCheck(path.Join(mysqlShellBackupLocation, "test")), tt.err)
		})
	}

}

func TestMySQLShellBackupRestorePreCheck(t *testing.T) {
	original := mysqlShellLoadFlags
	defer func() { mysqlShellLoadFlags = original }()

	engine := MySQLShellBackupEngine{}
	tests := []struct {
		name              string
		flags             string
		err               error
		shouldDeleteUsers bool
	}{
		{
			"empty load flags",
			`{}`,
			ErrMySQLShellPreCheck,
			false,
		},
		{
			"only updateGtidSet",
			`{"updateGtidSet": "replace"}`,
			ErrMySQLShellPreCheck,
			false,
		},
		{
			"only progressFile",
			`{"progressFile": ""}`,
			ErrMySQLShellPreCheck,
			false,
		},
		{
			"both values but unsupported values",
			`{"updateGtidSet": "append", "progressFile": "/tmp/test1"}`,
			ErrMySQLShellPreCheck,
			false,
		},
		{
			"supported values",
			`{"updateGtidSet": "replace", "progressFile": "", "skipBinlog": true, "loadUsers": false}`,
			nil,
			false,
		},
		{
			"should delete users",
			`{"updateGtidSet": "replace", "progressFile": "", "skipBinlog": true, "loadUsers": true}`,
			nil,
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mysqlShellLoadFlags = tt.flags
			shouldDeleteUsers, err := engine.restorePreCheck(context.Background(), RestoreParams{})
			assert.ErrorIs(t, err, tt.err)
			assert.Equal(t, tt.shouldDeleteUsers, shouldDeleteUsers)
		})
	}

}

func TestMySQLShellBackupRestorePreCheckDisableRedolog(t *testing.T) {
	original := mysqlShellSpeedUpRestore
	defer func() { mysqlShellSpeedUpRestore = original }()

	mysqlShellSpeedUpRestore = true
	engine := MySQLShellBackupEngine{}

	tests := []struct {
		version string
		err     error
	}{
		{
			version: "mysqld  Ver 5.7.27-0ubuntu0.19.04.1 for Linux on x86_64 ((Ubuntu))",
			err:     ErrMySQLShellPreCheck,
		},
		{
			version: "mysqld  Ver 5.7.26 for linux-glibc2.12 on x86_64 (MySQL Community Server (GPL))",
			err:     ErrMySQLShellPreCheck,
		},
		{
			version: "mysqld  Ver 5.7.26-29 for Linux on x86_64 (Percona Server (GPL), Release 29, Revision 11ad961)",
			err:     ErrMySQLShellPreCheck,
		},
		{
			version: "mysqld  Ver 8.0.16 for linux-glibc2.12 on x86_64 (MySQL Community Server - GPL)",
			err:     ErrMySQLShellPreCheck,
		},
		{
			version: "mysqld  Ver 8.0.15-6 for Linux on x86_64 (Percona Server (GPL), Release 6, Revision 63abd08)",
			err:     ErrMySQLShellPreCheck,
		},
		{
			version: "mysqld  Ver 8.0.42-0ubuntu0.22.04.1 for Linux on x86_64 ((Ubuntu))",
			err:     nil,
		},
		{
			version: "mysqld  Ver 8.0.42-33 for Linux on x86_64 (Percona Server (GPL), Release '33', Revision '9dc49998')",
			err:     nil,
		},
	}

	fakedb := fakesqldb.New(t)
	defer fakedb.Close()
	fakeMysqld := NewFakeMysqlDaemon(fakedb)
	defer fakeMysqld.Close()

	params := RestoreParams{
		Mysqld: fakeMysqld,
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			fakeMysqld.Version = tt.version

			_, err := engine.restorePreCheck(context.Background(), params)
			require.ErrorIs(t, err, tt.err)
		})
	}

}

func TestShouldDrainForBackupMySQLShell(t *testing.T) {
	original := mysqlShellBackupShouldDrain
	defer func() { mysqlShellBackupShouldDrain = original }()

	engine := MySQLShellBackupEngine{}

	mysqlShellBackupShouldDrain = false

	assert.False(t, engine.ShouldDrainForBackup(nil))
	assert.False(t, engine.ShouldDrainForBackup(&tabletmanagerdatapb.BackupRequest{}))

	mysqlShellBackupShouldDrain = true

	assert.True(t, engine.ShouldDrainForBackup(nil))
	assert.True(t, engine.ShouldDrainForBackup(&tabletmanagerdatapb.BackupRequest{}))
}

func TestCleanupMySQL(t *testing.T) {
	type userRecord struct {
		user, host string
	}

	tests := []struct {
		name              string
		existingDBs       []string
		expectedDropDBs   []string
		currentUser       string
		existingUsers     []userRecord
		expectedDropUsers []string
		shouldDeleteUsers bool
	}{
		{
			name:            "testing only specific DBs",
			existingDBs:     []string{"_vt", "vt_test"},
			expectedDropDBs: []string{"_vt", "vt_test"},
		},
		{
			name:            "testing with internal dbs",
			existingDBs:     []string{"_vt", "mysql", "vt_test", "performance_schema"},
			expectedDropDBs: []string{"_vt", "vt_test"},
		},
		{
			name:            "with users but without delete",
			existingDBs:     []string{"_vt", "mysql", "vt_test", "performance_schema"},
			expectedDropDBs: []string{"_vt", "vt_test"},
			existingUsers: []userRecord{
				{"test", "localhost"},
				{"app", "10.0.0.1"},
			},
			expectedDropUsers: []string{},
			shouldDeleteUsers: false,
		},
		{
			name:            "with users and delete",
			existingDBs:     []string{"_vt", "mysql", "vt_test", "performance_schema"},
			expectedDropDBs: []string{"_vt", "vt_test"},
			existingUsers: []userRecord{
				{"test", "localhost"},
				{"app", "10.0.0.1"},
			},
			expectedDropUsers: []string{"'test'@'localhost'", "'app'@'10.0.0.1'"},
			shouldDeleteUsers: true,
		},
		{
			name:            "with reserved users",
			existingDBs:     []string{"_vt", "mysql", "vt_test", "performance_schema"},
			expectedDropDBs: []string{"_vt", "vt_test"},
			existingUsers: []userRecord{
				{"mysql.sys", "localhost"},
				{"mysql.infoschema", "localhost"},
				{"mysql.session", "localhost"},
				{"test", "localhost"},
				{"app", "10.0.0.1"},
			},
			expectedDropUsers: []string{"'test'@'localhost'", "'app'@'10.0.0.1'"},
			shouldDeleteUsers: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakedb := fakesqldb.New(t)
			defer fakedb.Close()
			mysql := NewFakeMysqlDaemon(fakedb)
			defer mysql.Close()

			databases := [][]sqltypes.Value{}
			for _, db := range tt.existingDBs {
				databases = append(databases, []sqltypes.Value{sqltypes.NewVarChar(db)})
			}

			users := [][]sqltypes.Value{}
			for _, record := range tt.existingUsers {
				users = append(users, []sqltypes.Value{sqltypes.NewVarChar(record.user), sqltypes.NewVarChar(record.host)})
			}

			mysql.FetchSuperQueryMap = map[string]*sqltypes.Result{
				"SHOW DATABASES":                    {Rows: databases},
				"SELECT user()":                     {Rows: [][]sqltypes.Value{{sqltypes.NewVarChar(tt.currentUser)}}},
				"SELECT user, host FROM mysql.user": {Rows: users},
			}

			for _, drop := range tt.expectedDropDBs {
				mysql.ExpectedExecuteSuperQueryList = append(mysql.ExpectedExecuteSuperQueryList,
					fmt.Sprintf("DROP DATABASE IF EXISTS `%s`", drop),
				)
			}

			if tt.shouldDeleteUsers {
				for _, drop := range tt.expectedDropUsers {
					mysql.ExpectedExecuteSuperQueryList = append(mysql.ExpectedExecuteSuperQueryList,
						fmt.Sprintf("DROP USER %s", drop),
					)
				}
			}

			params := RestoreParams{
				Mysqld: mysql,
				Logger: logutil.NewMemoryLogger(),
			}

			err := cleanupMySQL(context.Background(), params, tt.shouldDeleteUsers)
			require.NoError(t, err)

			require.Equal(t, len(tt.expectedDropDBs)+len(tt.expectedDropUsers), mysql.ExpectedExecuteSuperQueryCurrent,
				"unexpected number of queries executed")
		})
	}

}

// this is a helper to write files in a temporary directory
func generateTestFile(t *testing.T, name, contents string) {
	f, err := os.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0700)
	require.NoError(t, err)
	defer f.Close()
	_, err = f.WriteString(contents)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// This tests if we are properly releasing the global read lock we acquire
// during ExecuteBackup(), even if the backup didn't succeed.
func TestMySQLShellBackupEngine_ExecuteBackup_ReleaseLock(t *testing.T) {
	originalLocation := mysqlShellBackupLocation
	mysqlShellBackupLocation = "logical"

	defer func() { // restore the original value.
		mysqlShellBackupLocation = originalLocation
	}()

	logger := logutil.NewMemoryLogger()
	fakedb := fakesqldb.New(t)
	defer fakedb.Close()
	mysql := NewFakeMysqlDaemon(fakedb)
	defer mysql.Close()

	params := BackupParams{
		TabletAlias: "test",
		Logger:      logger,
		Mysqld:      mysql,
	}
	bs := FakeBackupStorage{
		StartBackupReturn: FakeBackupStorageStartBackupReturn{},
	}

	t.Run("lock released if we see the mysqlsh lock being acquired", func(t *testing.T) {
		be := &MySQLShellBackupEngine{binaryName: path.Join(t.TempDir(), "mysqlsh.sh")}
		logger.Clear()
		manifestBuffer := ioutil.NewBytesBufferWriter()
		bs.StartBackupReturn.BackupHandle = &FakeBackupHandle{
			Dir:           t.TempDir(),
			AddFileReturn: FakeBackupHandleAddFileReturn{WriteCloser: manifestBuffer},
		}

		// this simulates mysql shell completing without any issues.
		generateTestFile(t, be.binaryName, fmt.Sprintf(
			"#!/bin/bash\n>&2 echo %s; echo \"backup completed\"; sleep 0.01", mysqlShellLockMessage))

		bh, err := bs.StartBackup(context.Background(), t.TempDir(), t.Name())
		require.NoError(t, err)

		_, err = be.ExecuteBackup(context.Background(), params, bh)
		require.NoError(t, err)
		require.False(t, mysql.GlobalReadLock) // lock must be released.

		// check the manifest is valid.
		var manifest MySQLShellBackupManifest
		err = json.Unmarshal(manifestBuffer.Bytes(), &manifest)
		require.NoError(t, err)

		require.Equal(t, mysqlShellBackupEngineName, manifest.BackupMethod)

		// did we notice the lock was release and did we release it ours as well?
		require.Contains(t, logger.String(), "global read lock released after",
			"failed to release the global lock after mysqlsh")
	})

	t.Run("lock released if we don't see mysqlsh release it", func(t *testing.T) {
		be := &MySQLShellBackupEngine{binaryName: path.Join(t.TempDir(), "mysqlsh.sh")}
		mysql.GlobalReadLock = false // clear lock status.
		logger.Clear()
		manifestBuffer := ioutil.NewBytesBufferWriter()
		bs.StartBackupReturn.BackupHandle = &FakeBackupHandle{
			Dir:           t.TempDir(),
			AddFileReturn: FakeBackupHandleAddFileReturn{WriteCloser: manifestBuffer},
		}

		// this simulates mysqlshell completing, but we don't see the message that is released its lock.
		generateTestFile(t, be.binaryName, "#!/bin/bash\nexit 0")

		bh, err := bs.StartBackup(context.Background(), t.TempDir(), t.Name())
		require.NoError(t, err)

		// in this case the backup was successful, but even if we didn't see mysqlsh release its lock
		// we make sure it is released at the end.
		_, err = be.ExecuteBackup(context.Background(), params, bh)
		require.NoError(t, err)
		require.False(t, mysql.GlobalReadLock) // lock must be released.

		// make sure we are at least logging the lock wasn't able to be released earlier.
		require.Contains(t, logger.String(), "could not release global lock earlier",
			"failed to log error message when unable to release lock during backup")
	})

	t.Run("lock released when backup fails", func(t *testing.T) {
		be := &MySQLShellBackupEngine{binaryName: path.Join(t.TempDir(), "mysqlsh.sh")}
		mysql.GlobalReadLock = false // clear lock status.
		logger.Clear()
		manifestBuffer := ioutil.NewBytesBufferWriter()
		bs.StartBackupReturn.BackupHandle = &FakeBackupHandle{
			Dir:           t.TempDir(),
			AddFileReturn: FakeBackupHandleAddFileReturn{WriteCloser: manifestBuffer},
		}

		// this simulates the backup process failing.
		generateTestFile(t, be.binaryName, "#!/bin/bash\nexit 1")

		bh, err := bs.StartBackup(context.Background(), t.TempDir(), t.Name())
		require.NoError(t, err)

		_, err = be.ExecuteBackup(context.Background(), params, bh)
		require.ErrorContains(t, err, "mysqlshell failed")
		require.False(t, mysql.GlobalReadLock) // lock must be released.
	})

}
