package server

import (
	"fmt"
	"io/ioutil"
	"path/filepath"

	"github.com/influxdb/influxdb/cluster"
	"github.com/influxdb/influxdb/configuration"
	"github.com/influxdb/influxdb/coordinator"
	"github.com/influxdb/influxdb/datastore"
	// "github.com/influxdb/influxdb/datastore/storage"
	"github.com/influxdb/influxdb/engine"
	"github.com/influxdb/influxdb/metastore"
	"github.com/influxdb/influxdb/parser"
	"github.com/influxdb/influxdb/protocol"

	log "code.google.com/p/log4go"
	// "github.com/BurntSushi/toml"
	"github.com/jmhodges/levigo"
)

// Used for migrating data from old versions of influx.
type DataMigrator struct {
	baseDbDir     string
	dbDir         string
	metaStore     *metastore.Store
	config        *configuration.Configuration
	clusterConfig *cluster.ClusterConfiguration
	coord         *coordinator.CoordinatorImpl
}

const (
	MIGRATED_MARKER = "MIGRATED"
	OLD_SHARD_DIR   = "shard_db"
)

var (
	endStreamResponse = protocol.Response_END_STREAM
)

func NewDataMigrator(coord *coordinator.CoordinatorImpl, clusterConfig *cluster.ClusterConfiguration, config *configuration.Configuration, baseDbDir, newSubDir string, metaStore *metastore.Store) *DataMigrator {
	return &DataMigrator{
		baseDbDir:     baseDbDir,
		dbDir:         filepath.Join(baseDbDir, OLD_SHARD_DIR),
		metaStore:     metaStore,
		config:        config,
		clusterConfig: clusterConfig,
		coord:         coord,
	}
}

func (dm *DataMigrator) Migrate() {
	log.Info("Migrating from dir %s", dm.dbDir)
	infos, err := ioutil.ReadDir(dm.dbDir)
	if err != nil {
		log.Error("Error Migrating: ", err)
		return
	}
	//  go through in reverse order so most recently created shards will be migrated first
	for i := len(infos) - 1; i >= 0; i-- {
		info := infos[i]
		if info.IsDir() {
			dm.migrateDir(info.Name())
		}
	}
}

func (dm *DataMigrator) migrateDir(name string) {
	log.Info("Migrating %s", name)
	shard, err := dm.getShard(name)
	if err != nil {
		log.Error("Error migrating: %s", err.Error())
		return
	}
	defer shard.Close()
	databases := dm.clusterConfig.GetDatabases()
	for _, database := range databases {
		err := dm.migrateDatabaseInShard(database.Name, shard)
		if err != nil {
			log.Error("Error migrating database %s: %s", database.Name, err.Error())
			return
		}
	}
}

func (dm *DataMigrator) migrateDatabaseInShard(database string, shard *datastore.LevelDbShard) error {
	log.Info("Migrating database %s for shard", database)
	seriesNames := shard.GetSeriesForDatabase(database)
	log.Info("Migrating %d series", len(seriesNames))

	admin := dm.clusterConfig.GetClusterAdmin(dm.clusterConfig.GetClusterAdmins()[0])
	for _, series := range seriesNames {
		q, err := parser.ParseQuery(fmt.Sprintf("select * from \"%s\"", series))
		if err != nil {
			log.Error("Problem migrating series %s", series)
			continue
		}
		query := q[0]
		seriesChan := make(chan *protocol.Response)
		queryEngine := engine.NewPassthroughEngine(seriesChan, 2000)
		querySpec := parser.NewQuerySpec(admin, database, query)
		go func() {
			err := shard.Query(querySpec, queryEngine)
			if err != nil {
				log.Error("Error migrating %s", err.Error())
			}
			queryEngine.Close()
			seriesChan <- &protocol.Response{Type: &endStreamResponse}
		}()
		for {
			response := <-seriesChan
			if *response.Type == endStreamResponse {
				break
			}
			err := dm.coord.WriteSeriesData(admin, database, []*protocol.Series{response.Series})
			if err != nil {
				log.Error("Writing Series data: %s", err.Error())
			}
		}
	}
	return nil
}

func (dm *DataMigrator) getShard(name string) (*datastore.LevelDbShard, error) {
	dbDir := filepath.Join(dm.baseDbDir, OLD_SHARD_DIR, name)
	cache := levigo.NewLRUCache(int(2000))
	opts := levigo.NewOptions()
	opts.SetCache(cache)
	opts.SetCreateIfMissing(true)
	opts.SetMaxOpenFiles(1000)
	ldb, err := levigo.Open(dbDir, opts)
	if err != nil {
		return nil, err
	}

	return datastore.NewLevelDbShard(ldb, dm.config.StoragePointBatchSize, dm.config.StorageWriteBatchSize)

	// // old shards will only be leveldb type shards
	// engine := "leveldb"
	// init, err := storage.GetInitializer(engine)
	// if err != nil {
	// 	log.Error("Error opening shard: ", err)
	// 	return nil, err
	// }
	// c := init.NewConfig()
	// conf, ok := dm.config.StorageEngineConfigs[engine]
	// if err := toml.PrimitiveDecode(conf, c); ok && err != nil {
	// 	return nil, err
	// }

	// // TODO: this is for backward compatability with the old
	// // configuration
	// if leveldbConfig, ok := c.(*storage.LevelDbConfiguration); ok {
	// 	if leveldbConfig.LruCacheSize == 0 {
	// 		leveldbConfig.LruCacheSize = configuration.Size(dm.config.LevelDbLruCacheSize)
	// 	}

	// 	if leveldbConfig.MaxOpenFiles == 0 {
	// 		leveldbConfig.MaxOpenFiles = dm.config.LevelDbMaxOpenFiles
	// 	}
	// }

	// se, err := init.Initialize(dbDir, c)
	// db, err := datastore.NewShard(se, dm.config.StoragePointBatchSize, dm.config.StorageWriteBatchSize, dm.metaStore)
	// if err != nil {
	// 	log.Error("Error creating shard: ", err)
	// 	se.Close()
	// 	return nil, err
	// }
	// return db, nil
}
