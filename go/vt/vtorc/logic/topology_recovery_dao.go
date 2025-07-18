/*
   Copyright 2015 Shlomi Noach, courtesy Booking.com

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

package logic

import (
	"errors"
	"fmt"
	"strings"

	"vitess.io/vitess/go/vt/external/golib/sqlutils"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/vtorc/config"
	"vitess.io/vitess/go/vt/vtorc/db"
	"vitess.io/vitess/go/vt/vtorc/inst"
)

// InsertRecoveryDetection inserts the recovery analysis that has been detected.
func InsertRecoveryDetection(analysisEntry *inst.ReplicationAnalysis) error {
	sqlResult, err := db.ExecVTOrc(`INSERT OR IGNORE
		INTO recovery_detection (
			alias,
			analysis,
			keyspace,
			shard,
			detection_timestamp
		) VALUES (
			?,
			?,
			?,
			?,
			DATETIME('now')
		)`,
		analysisEntry.AnalyzedInstanceAlias,
		string(analysisEntry.Analysis),
		analysisEntry.AnalyzedKeyspace,
		analysisEntry.AnalyzedShard,
	)
	if err != nil {
		log.Error(err)
		return err
	}
	id, err := sqlResult.LastInsertId()
	if err != nil {
		log.Error(err)
		return err
	}
	analysisEntry.RecoveryId = id
	return nil
}

func writeTopologyRecovery(topologyRecovery *TopologyRecovery) (*TopologyRecovery, error) {
	analysisEntry := topologyRecovery.AnalysisEntry
	sqlResult, err := db.ExecVTOrc(`INSERT OR IGNORE
		INTO topology_recovery (
			recovery_id,
			alias,
			start_recovery,
			analysis,
			keyspace,
			shard,
			detection_id
		) VALUES (
			?,
			?,
			DATETIME('now'),
			?,
			?,
			?,
			?
		)`,
		sqlutils.NilIfZero(topologyRecovery.ID),
		analysisEntry.AnalyzedInstanceAlias,
		string(analysisEntry.Analysis),
		analysisEntry.AnalyzedKeyspace,
		analysisEntry.AnalyzedShard,
		analysisEntry.AnalyzedInstanceAlias,
		analysisEntry.RecoveryId,
	)
	if err != nil {
		return nil, err
	}
	rows, err := sqlResult.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, nil
	}
	lastInsertID, err := sqlResult.LastInsertId()
	if err != nil {
		return nil, err
	}
	topologyRecovery.ID = lastInsertID
	return topologyRecovery, nil
}

// AttemptRecoveryRegistration tries to add a recovery entry; if this fails that means recovery is already in place.
func AttemptRecoveryRegistration(analysisEntry *inst.ReplicationAnalysis) (*TopologyRecovery, error) {
	// Check if there is an active recovery in progress for the cluster of the given instance.
	recoveries, err := ReadActiveClusterRecoveries(analysisEntry.AnalyzedKeyspace, analysisEntry.AnalyzedShard)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	if len(recoveries) > 0 {
		errMsg := fmt.Sprintf("AttemptRecoveryRegistration: Active recovery (id:%v) in the cluster %s:%s for %s", recoveries[0].ID, analysisEntry.AnalyzedKeyspace, analysisEntry.AnalyzedShard, recoveries[0].AnalysisEntry.Analysis)
		log.Errorf(errMsg)
		return nil, errors.New(errMsg)
	}

	topologyRecovery := NewTopologyRecovery(*analysisEntry)

	topologyRecovery, err = writeTopologyRecovery(topologyRecovery)
	if err != nil {
		log.Error(err)
		return nil, err
	}
	return topologyRecovery, nil
}

// ResolveRecovery is called on completion of a recovery process and updates the recovery status.
// It does not clear the "active period" as this still takes place in order to avoid flapping.
func writeResolveRecovery(topologyRecovery *TopologyRecovery) error {
	_, err := db.ExecVTOrc(`UPDATE topology_recovery
		SET
			is_successful = ?,
			successor_alias = ?,
			all_errors = ?,
			end_recovery = DATETIME('now')
		WHERE
			recovery_id = ?
		`,
		topologyRecovery.IsSuccessful,
		topologyRecovery.SuccessorAlias,
		strings.Join(topologyRecovery.AllErrors, "\n"),
		topologyRecovery.ID,
	)
	if err != nil {
		log.Error(err)
	}
	return err
}

// readRecoveries reads recovery entry/audit entries from topology_recovery
func readRecoveries(whereCondition string, limit string, args []any) ([]*TopologyRecovery, error) {
	res := []*TopologyRecovery{}
	query := fmt.Sprintf(`SELECT
			recovery_id,
			alias,
			start_recovery,
			IFNULL(end_recovery, '') AS end_recovery,
			is_successful,
			IFNULL(successor_alias, '') AS successor_alias,
			analysis,
			keyspace,
			shard,
			all_errors,
			detection_id
		FROM
			topology_recovery
		%s
		ORDER BY recovery_id DESC
		%s
		`,
		whereCondition,
		limit,
	)
	err := db.QueryVTOrc(query, args, func(m sqlutils.RowMap) error {
		topologyRecovery := *NewTopologyRecovery(inst.ReplicationAnalysis{})
		topologyRecovery.ID = m.GetInt64("recovery_id")

		topologyRecovery.RecoveryStartTimestamp = m.GetString("start_recovery")
		topologyRecovery.RecoveryEndTimestamp = m.GetString("end_recovery")
		topologyRecovery.IsSuccessful = m.GetBool("is_successful")

		topologyRecovery.AnalysisEntry.AnalyzedInstanceAlias = m.GetString("alias")
		topologyRecovery.AnalysisEntry.Analysis = inst.AnalysisCode(m.GetString("analysis"))
		topologyRecovery.AnalysisEntry.AnalyzedKeyspace = m.GetString("keyspace")
		topologyRecovery.AnalysisEntry.AnalyzedShard = m.GetString("shard")

		topologyRecovery.SuccessorAlias = m.GetString("successor_alias")

		topologyRecovery.AllErrors = strings.Split(m.GetString("all_errors"), "\n")

		topologyRecovery.DetectionID = m.GetInt64("detection_id")

		res = append(res, &topologyRecovery)
		return nil
	})

	if err != nil {
		log.Error(err)
	}
	return res, err
}

// ReadActiveClusterRecoveries reads recoveries that are ongoing for the given cluster.
func ReadActiveClusterRecoveries(keyspace string, shard string) ([]*TopologyRecovery, error) {
	whereClause := `WHERE
		end_recovery IS NULL
		AND keyspace = ?
		AND shard = ?`
	return readRecoveries(whereClause, ``, sqlutils.Args(keyspace, shard))
}

// ReadRecentRecoveries reads latest recovery entries from topology_recovery
func ReadRecentRecoveries(page int) ([]*TopologyRecovery, error) {
	whereConditions := []string{}
	whereClause := ""
	var args []any
	if len(whereConditions) > 0 {
		whereClause = fmt.Sprintf("WHERE %s", strings.Join(whereConditions, " AND "))
	}
	limit := `LIMIT ? OFFSET ?`
	args = append(args, config.AuditPageSize, page*config.AuditPageSize)
	return readRecoveries(whereClause, limit, args)
}

// writeTopologyRecoveryStep writes down a single step in a recovery process
func writeTopologyRecoveryStep(topologyRecoveryStep *TopologyRecoveryStep) error {
	sqlResult, err := db.ExecVTOrc(`INSERT OR IGNORE
		INTO topology_recovery_steps (
			recovery_step_id,
			recovery_id,
			audit_at,
			message
		) VALUES (
			?,
			?,
			DATETIME('now'),
			?
		)`,
		sqlutils.NilIfZero(topologyRecoveryStep.ID),
		topologyRecoveryStep.RecoveryID,
		topologyRecoveryStep.Message,
	)
	if err != nil {
		log.Error(err)
		return err
	}
	topologyRecoveryStep.ID, err = sqlResult.LastInsertId()
	if err != nil {
		log.Error(err)
	}
	return err
}

// ExpireRecoveryDetectionHistory removes old rows from the recovery_detection table
func ExpireRecoveryDetectionHistory() error {
	return inst.ExpireTableData("recovery_detection", "detection_timestamp")
}

// ExpireTopologyRecoveryHistory removes old rows from the topology_recovery table
func ExpireTopologyRecoveryHistory() error {
	return inst.ExpireTableData("topology_recovery", "start_recovery")
}

// ExpireTopologyRecoveryStepsHistory removes old rows from the topology_recovery_steps table
func ExpireTopologyRecoveryStepsHistory() error {
	return inst.ExpireTableData("topology_recovery_steps", "audit_at")
}
