// Licensed to LinDB under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. LinDB licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package brokerquery

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/lindb/lindb/aggregation"
	"github.com/lindb/lindb/constants"
	"github.com/lindb/lindb/models"
	"github.com/lindb/lindb/pkg/timeutil"
	"github.com/lindb/lindb/query"
	"github.com/lindb/lindb/series"
	"github.com/lindb/lindb/series/tag"
	"github.com/lindb/lindb/sql/stmt"
)

// metricQuery implements MetricQuery.
type metricQuery struct {
	queryFactory *queryFactory

	ctx      context.Context
	database string
	root     models.Node

	startTime   time.Time
	endPlanTime time.Time

	stmtQuery  *stmt.Query
	plan       *brokerPlan
	expression *aggregation.Expression
}

// newMetricQuery creates the execution which executes the job of parallel query.
func newMetricQuery(
	ctx context.Context,
	root models.Node,
	database string,
	sql *stmt.Query,
	queryFactory *queryFactory,
) MetricQuery {
	return &metricQuery{
		stmtQuery:    sql,
		root:         root,
		database:     database,
		ctx:          ctx,
		queryFactory: queryFactory,
	}
}

// makePlan executes search logic in broker level,
// 1) get metadata based on params
// 2) build execute plan
func (mq *metricQuery) makePlan() error {
	startTime := time.Now()
	databaseCfg, ok := mq.queryFactory.stateMgr.GetDatabaseCfg(mq.database)
	if !ok {
		return query.ErrDatabaseNotExist
	}

	// FIXME: need using storage's replica state ???
	storageNodes, err := mq.queryFactory.stateMgr.GetQueryableReplicas(mq.database)
	if err != nil {
		return err
	}
	if len(storageNodes) == 0 {
		return constants.ErrReplicaNotFound
	}
	brokerNodes := mq.queryFactory.stateMgr.GetLiveNodes()

	mq.plan = newBrokerPlan(
		mq.stmtQuery,
		databaseCfg,
		storageNodes,
		mq.queryFactory.stateMgr.GetCurrentNode(),
		brokerNodes,
	)
	if err := mq.plan.Plan(); err != nil {
		return err
	}

	mq.startTime = startTime
	mq.plan.physicalPlan.Database = mq.database
	mq.stmtQuery = mq.plan.query
	mq.expression = aggregation.NewExpression(
		mq.plan.query.TimeRange,
		mq.plan.query.Interval.Int64(),
		mq.plan.query.SelectItems,
	)
	return nil
}

// WaitResponse builds the plan, the dispatch the task by task-manager
func (mq *metricQuery) WaitResponse() (*models.ResultSet, error) {
	if err := mq.makePlan(); err != nil {
		return nil, err
	}
	mq.endPlanTime = time.Now()

	eventCh, err := mq.queryFactory.taskManager.SubmitMetricTask(
		mq.ctx,
		mq.plan.physicalPlan,
		mq.plan.query,
	)
	// send error
	if err != nil {
		return nil, err
	}
	var (
		event *series.TimeSeriesEvent
		ok    bool
	)
	select {
	case event, ok = <-eventCh:
		if !ok {
			return nil, fmt.Errorf("missing response from sent tasks")
		}
		if event.Err != nil {
			return nil, event.Err
		}
	case <-mq.ctx.Done():
		return nil, ErrTimeout
	}

	return mq.makeResultSet(event), nil
}

func (mq *metricQuery) makeResultSet(event *series.TimeSeriesEvent) (resultSet *models.ResultSet) {
	makeResultStartTime := time.Now()

	resultSet = new(models.ResultSet)
	// TODO: merge stats for cross idc query?
	groupByKeys := mq.stmtQuery.GroupBy
	groupByKeysLength := len(groupByKeys)
	fieldsMap := make(map[string]struct{})
	for _, ts := range event.SeriesList {
		var tags map[string]string
		var tagValues string
		if groupByKeysLength > 0 {
			tagValues = ts.Tags()
			tagValues := tag.SplitTagValues(tagValues)
			if groupByKeysLength != len(tagValues) {
				// if tag values not match group by tag keys, ignore this time series
				continue
			}
			// build group by tags for final result
			tags = make(map[string]string)
			for idx, tagKey := range groupByKeys {
				tags[tagKey] = tagValues[idx]
			}
		}
		timeSeries := models.NewSeries(tags, tagValues)
		resultSet.AddSeries(timeSeries)
		mq.expression.Eval(ts)
		rs := mq.expression.ResultSet()
		for fieldName, values := range rs {
			if values == nil {
				continue
			}
			points := models.NewPoints()
			it := values.NewIterator()
			for it.HasNext() {
				slot, val := it.Next()
				if math.IsNaN(val) {
					// TODO: need check
					continue
				}
				points.AddPoint(timeutil.CalcTimestamp(mq.stmtQuery.TimeRange.Start, slot, mq.stmtQuery.Interval), val)
			}
			timeSeries.AddField(fieldName, points)
			fieldsMap[fieldName] = struct{}{}
		}
		mq.expression.Reset()
	}

	sort.Slice(resultSet.Series, func(i, j int) bool {
		return resultSet.Series[i].TagValues < resultSet.Series[j].TagValues
	})

	resultSet.MetricName = mq.stmtQuery.MetricName
	resultSet.GroupBy = mq.stmtQuery.GroupBy
	for fName := range fieldsMap {
		resultSet.Fields = append(resultSet.Fields, fName)
	}
	resultSet.StartTime = mq.stmtQuery.TimeRange.Start
	resultSet.EndTime = mq.stmtQuery.TimeRange.End
	resultSet.Interval = mq.stmtQuery.Interval.Int64()

	resultSet.Stats = event.Stats
	if resultSet.Stats != nil {
		now := time.Now()
		resultSet.Stats.Root = mq.root.Indicator()
		resultSet.Stats.PlanCost = mq.endPlanTime.Sub(mq.startTime).Nanoseconds()
		resultSet.Stats.PlanStart = mq.startTime.UnixNano()
		resultSet.Stats.PlanEnd = mq.endPlanTime.UnixNano()
		resultSet.Stats.WaitCost = makeResultStartTime.Sub(mq.endPlanTime).Nanoseconds()
		resultSet.Stats.WaitStart = mq.endPlanTime.UnixNano()
		resultSet.Stats.WaitEnd = makeResultStartTime.UnixNano()
		resultSet.Stats.ExpressCost = now.Sub(makeResultStartTime).Nanoseconds()
		resultSet.Stats.ExpressStart = makeResultStartTime.UnixNano()
		resultSet.Stats.ExpressEnd = now.UnixNano()
		resultSet.Stats.TotalCost = now.Sub(mq.startTime).Nanoseconds()
		resultSet.Stats.Start = mq.startTime.UnixNano()
		resultSet.Stats.End = now.UnixNano()
	}
	return resultSet
}
