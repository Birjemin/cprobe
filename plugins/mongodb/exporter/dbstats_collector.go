// mongodb_exporter
// Copyright (C) 2017 Percona LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exporter

import (
	"context"

	"github.com/cprobe/cprobe/lib/logger"
	"github.com/prometheus/client_golang/prometheus"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

type dbstatsCollector struct {
	ctx  context.Context
	base *baseCollector

	compatibleMode bool
	topologyInfo   labelsGetter

	databaseFilter []string

	freeStorage bool

	opts *Opts
}

// newDBStatsCollector creates a collector for statistics on database storage.
func newDBStatsCollector(ctx context.Context, client *mongo.Client, compatible bool, topology labelsGetter, databaseRegex []string, freeStorage bool, opts *Opts) *dbstatsCollector {
	return &dbstatsCollector{
		ctx:  ctx,
		base: newBaseCollector(client),

		compatibleMode: compatible,
		topologyInfo:   topology,

		databaseFilter: databaseRegex,

		freeStorage: freeStorage,

		opts: opts,
	}
}

func (d *dbstatsCollector) Describe(ch chan<- *prometheus.Desc) {
	d.base.Describe(d.ctx, ch, d.collect)
}

func (d *dbstatsCollector) Collect(ch chan<- prometheus.Metric) {
	d.base.Collect(ch)
}

func (d *dbstatsCollector) collect(ch chan<- prometheus.Metric) {
	defer measureCollectTime(ch, "mongodb", "dbstats")()

	client := d.base.client

	dbNames, err := databases(d.ctx, client, d.databaseFilter, nil)
	if err != nil {
		logger.Errorf("Failed to get database names: %s uri: %v", err, d.opts.URI)
		return
	}

	for _, db := range dbNames {
		var dbStats bson.M
		var cmd bson.D
		if d.freeStorage {
			cmd = bson.D{{Key: "dbStats", Value: 1}, {Key: "scale", Value: 1}, {Key: "freeStorage", Value: 1}}
		} else {
			cmd = bson.D{{Key: "dbStats", Value: 1}, {Key: "scale", Value: 1}}
		}
		r := client.Database(db).RunCommand(d.ctx, cmd)
		err := r.Decode(&dbStats)
		if err != nil {
			logger.Errorf("Failed to get $dbstats for database %s: %s", db, err)

			continue
		}

		prefix := "dbstats"

		labels := d.topologyInfo.baseLabels()

		// Since all dbstats will have the same fields, we need to use a label
		// to differentiate metrics between different databases.
		labels["database"] = db

		newMetrics := makeMetrics(prefix, dbStats, labels, d.compatibleMode)
		for _, metric := range newMetrics {
			ch <- metric
		}
	}
}

var _ prometheus.Collector = (*dbstatsCollector)(nil)
