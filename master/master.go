// Copyright 2020 gorse Project Authors
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
package master

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ReneKroon/ttlcache/v2"
	"github.com/araddon/dateparse"
	log "github.com/sirupsen/logrus"
	"github.com/zhenghaoz/gorse/base"
	"github.com/zhenghaoz/gorse/config"
	"github.com/zhenghaoz/gorse/model/cf"
	"github.com/zhenghaoz/gorse/model/rank"
	"github.com/zhenghaoz/gorse/protocol"
	"github.com/zhenghaoz/gorse/storage/cache"
	"github.com/zhenghaoz/gorse/storage/data"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

const (
	ServerNode = "server"
	WorkerNode = "worker"
)

type Master struct {
	protocol.UnimplementedMasterServer

	// cluster meta cache
	ttlCache   *ttlcache.Cache
	nodesMap   map[string]string
	nodesMutex sync.Mutex

	// configuration
	cfg  *config.Config
	meta *toml.MetaData

	// database connection
	dataStore  data.Database
	cacheStore cache.Database

	// match model
	cfModel           cf.MatrixFactorization
	matchModelVersion int
	matchModelMutex   sync.Mutex

	// rank model
	rankModel        rank.FactorizationMachine
	rankModelVersion int
	rankModelMutex   sync.Mutex
}

func NewMaster(cfg *config.Config, meta *toml.MetaData) *Master {
	l := &Master{
		nodesMap:          make(map[string]string),
		cfg:               cfg,
		meta:              meta,
		matchModelVersion: rand.Int(),
		rankModelVersion:  rand.Int(),
	}
	return l
}

func (m *Master) Serve() {

	// create cluster meta cache
	m.ttlCache = ttlcache.NewCache()
	m.ttlCache.SetExpirationCallback(m.NodeDown)
	m.ttlCache.SetNewItemCallback(m.NodeUp)
	if err := m.ttlCache.SetTTL(
		time.Duration(m.cfg.Master.ClusterMetaTimeout) * time.Second,
	); err != nil {
		log.Error("master:", err)
	}

	// connect data database
	var err error
	m.dataStore, err = data.Open(m.cfg.Database.DataStore)
	if err != nil {
		log.Fatalf("master: failed to connect data database (%v)", err)
	}
	if err = m.dataStore.Init(); err != nil {
		log.Fatalf("master: failed to init database ")
	}

	// connect cache database
	m.cacheStore, err = cache.Open(m.cfg.Database.CacheStore)
	if err != nil {
		log.Fatalf("master: failed to connect cache database (%v)", err)
	}

	// start loop
	go m.Loop()

	// start rpc server
	log.Infof("master: start rpc server %v:%v", m.cfg.Master.Host, m.cfg.Master.Port)
	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", m.cfg.Master.Host, m.cfg.Master.Port))
	if err != nil {
		log.Fatalf("master: failed to listen: %v", err)
	}
	var opts []grpc.ServerOption
	grpcServer := grpc.NewServer(opts...)
	protocol.RegisterMasterServer(grpcServer, m)
	if err = grpcServer.Serve(lis); err != nil {
		log.Fatalf("master: failed to start rpc server (%v)", err)
	}
}

func (m *Master) GetConfig(context.Context, *protocol.Void) (*protocol.Config, error) {
	s, err := json.Marshal(m.cfg)
	if err != nil {
		return nil, err
	}
	return &protocol.Config{Json: string(s)}, nil
}

func (m *Master) RegisterServer(ctx context.Context, _ *protocol.Void) (*protocol.Void, error) {
	p, _ := peer.FromContext(ctx)
	addr := p.Addr.String()
	if err := m.ttlCache.Set(addr, ServerNode); err != nil {
		log.Errorf("master: failed to set ttlcache (%v)", err)
		return nil, err
	}
	return &protocol.Void{}, nil
}

func (m *Master) RegisterWorker(ctx context.Context, _ *protocol.Void) (*protocol.Void, error) {
	p, _ := peer.FromContext(ctx)
	addr := p.Addr.String()
	if err := m.ttlCache.Set(addr, WorkerNode); err != nil {
		log.Errorf("master: failed to set ttlcache (%v)", err)
		return nil, err
	}
	return &protocol.Void{}, nil
}

func (m *Master) GetCluster(ctx context.Context, _ *protocol.Void) (*protocol.Cluster, error) {
	cluster := &protocol.Cluster{
		Workers: make([]string, 0),
		Servers: make([]string, 0),
	}
	// add me
	p, _ := peer.FromContext(ctx)
	cluster.Me = p.Addr.String()
	// add master
	cluster.Master = fmt.Sprintf("%s:%d", m.cfg.Master.Host, m.cfg.Master.Port)
	// add servers/workers
	m.nodesMutex.Lock()
	defer m.nodesMutex.Unlock()
	for addr, nodeType := range m.nodesMap {
		switch nodeType {
		case WorkerNode:
			cluster.Workers = append(cluster.Workers, addr)
		case ServerNode:
			cluster.Servers = append(cluster.Servers, addr)
		default:
			log.Fatalf("master: unkown node (%v)", nodeType)
		}
	}
	return cluster, nil
}

func (m *Master) GetMatchModelVersion(context.Context, *protocol.Void) (*protocol.Model, error) {
	m.matchModelMutex.Lock()
	defer m.matchModelMutex.Unlock()
	// skip empty model
	if m.cfModel == nil {
		return &protocol.Model{Version: 0}, nil
	}
	return &protocol.Model{
		Name:    m.cfg.CF.CFModel,
		Version: int64(m.matchModelVersion),
	}, nil
}

func (m *Master) GetMatchModel(context.Context, *protocol.Void) (*protocol.Model, error) {
	m.matchModelMutex.Lock()
	defer m.matchModelMutex.Unlock()
	// skip empty model
	if m.cfModel == nil {
		return &protocol.Model{Version: 0}, nil
	}
	// encode model
	modelData, err := cf.EncodeModel(m.cfModel)
	if err != nil {
		return nil, err
	}
	return &protocol.Model{
		Name:    m.cfg.CF.CFModel,
		Version: int64(m.matchModelVersion),
		Model:   modelData,
	}, nil
}

func (m *Master) GetRankModelVersion(context.Context, *protocol.Void) (*protocol.Model, error) {
	m.rankModelMutex.Lock()
	defer m.rankModelMutex.Unlock()
	// skip empty model
	if m.rankModel == nil {
		return &protocol.Model{Version: 0}, nil
	}
	return &protocol.Model{Version: int64(m.rankModelVersion)}, nil
}

func (m *Master) GetRankModel(context.Context, *protocol.Void) (*protocol.Model, error) {
	m.rankModelMutex.Lock()
	defer m.rankModelMutex.Unlock()
	// skip empty model
	if m.rankModel == nil {
		return &protocol.Model{Version: 0}, nil
	}
	// encode model
	modelData, err := rank.EncodeModel(m.rankModel)
	if err != nil {
		return nil, err
	}
	return &protocol.Model{
		Version: int64(m.rankModelVersion),
		Model:   modelData,
	}, nil
}

func (m *Master) NodeUp(key string, value interface{}) {
	nodeType := value.(string)
	log.Infof("master: %s (%s) up", nodeType, key)
	m.nodesMutex.Lock()
	defer m.nodesMutex.Unlock()
	m.nodesMap[key] = nodeType
}

func (m *Master) NodeDown(key string, value interface{}) {
	nodeType := value.(string)
	log.Infof("master: %s (%s) down", nodeType, key)
	m.nodesMutex.Lock()
	defer m.nodesMutex.Unlock()
	delete(m.nodesMap, key)
}

func (m *Master) Loop() {
	// calculate loop period
	loopPeriod := base.GCD(
		m.cfg.CF.FitPeriod,
		m.cfg.Rank.FitPeriod,
		m.cfg.Similar.UpdatePeriod,
		m.cfg.Popular.UpdatePeriod,
		m.cfg.Latest.UpdatePeriod)
	log.Infof("master: start loop (period = %v min)", loopPeriod)

	for {
		// check stale
		isPopItemStale := m.IsStale(cache.CollectPopularTime, m.cfg.Popular.UpdatePeriod)
		isLatestStale := m.IsStale(cache.CollectLatestTime, m.cfg.Latest.UpdatePeriod)
		isSimilarStale := m.IsStale(cache.CollectSimilarTime, m.cfg.Similar.UpdatePeriod)
		isRankModelStale := m.IsStale(cache.LastFitRankModelTime, m.cfg.Rank.FitPeriod)
		isCFModelStale := m.IsStale(cache.LastFitCFModelTime, m.cfg.CF.FitPeriod)

		// pull dataset for rank
		if isRankModelStale || m.rankModel == nil {
			rankDataSet, err := rank.LoadDataFromDatabase(m.dataStore, m.cfg.Rank.FeedbackTypes)
			if err != nil {
				log.Fatalf("master: failed to pull dataset for ranking (%v)", err)
			}
			if rankDataSet.PositiveCount == 0 {
				log.Infof("master: empty dataset (feedback_type = %v)", m.cfg.Rank.FeedbackTypes)
			} else if err = m.FitRankModel(rankDataSet); err != nil {
				log.Fatalf("master: failed to renew ranking model (%v)", err)
			}
		}

		if isCFModelStale || isLatestStale || isPopItemStale || isSimilarStale || m.cfModel == nil {
			// download dataset
			log.Infof("master: load data from database")
			dataSet, items, feedbacks, err := cf.LoadDataFromDatabase(m.dataStore, m.cfg.CF.FeedbackTypes)
			if err != nil {
				log.Fatal("master: ", err)
			}
			if dataSet.Count() == 0 {
				log.Info("master: empty dataset")
			} else {
				log.Infof("master: data loaded (#user = %v, #item = %v, #feedback = %v)",
					dataSet.UserCount(), dataSet.ItemCount(), dataSet.Count())

				// collect popular items
				if isPopItemStale {
					m.CollectPopItem(items, feedbacks)
				}

				// collect latest items
				if isLatestStale {
					m.CollectLatest(items)
				}

				// collect similar items
				if isSimilarStale {
					m.CollectSimilar(items, dataSet)
				}

				if isCFModelStale || m.cfModel == nil {
					log.Infof("master: fit cf model (n_jobs = %v)", m.cfg.Master.Jobs)
					if err = m.FitCFModel(dataSet); err != nil {
						log.Errorf("master: failed to fit cf model (%v)", err)
					}
					log.Infof("master: completed fit cf model")
				}
			}
		}

		// sleep
		time.Sleep(time.Duration(loopPeriod) * time.Minute)
	}
}

func (m *Master) FitRankModel(dataSet *rank.Dataset) error {
	trainSet, testSet := dataSet.Split(0.2, 0)
	testSet.NegativeSample(1, trainSet, 0)
	nextModel := rank.NewFM(rank.FMTask(m.cfg.Rank.Task), nil)
	nextModel.Fit(trainSet, testSet, nil)

	m.rankModelMutex.Lock()
	m.rankModel = nextModel
	m.rankModelVersion++
	m.rankModelMutex.Unlock()

	if err := m.cacheStore.SetString(cache.GlobalMeta, cache.LastFitRankModelTime, base.Now()); err != nil {
		return err
	}
	return m.cacheStore.SetString(cache.GlobalMeta, cache.LatestRankModelVersion, fmt.Sprintf("%x", m.rankModelVersion))
}

func (m *Master) FitCFModel(dataSet *cf.DataSet) error {
	// training match model
	trainSet, testSet := dataSet.Split(m.cfg.CF.NumTestUsers, 0)
	nextModel, err := cf.NewModel(m.cfg.CF.CFModel, m.cfg.CF.GetParams(m.meta))
	if err != nil {
		return err
	}
	nextModel.Fit(trainSet, testSet, m.cfg.CF.GetFitConfig())

	// update match model
	m.matchModelMutex.Lock()
	m.cfModel = nextModel
	m.matchModelVersion++
	m.matchModelMutex.Unlock()

	if err = m.cacheStore.SetString(cache.GlobalMeta, cache.LastFitCFModelTime, base.Now()); err != nil {
		return err
	}
	return m.cacheStore.SetString(cache.GlobalMeta, cache.LatestCFModelVersion, fmt.Sprintf("%x", m.matchModelVersion))
}

func (m *Master) IsStale(dateTimeField string, timeLimit int) bool {
	updateTimeText, err := m.cacheStore.GetString(cache.GlobalMeta, dateTimeField)
	if err != nil {
		if err.Error() == "redis: nil" {
			return true
		}
		log.Fatalf("master: failed to get timestamp (%v)", err)
	}
	updateTime, err := dateparse.ParseAny(updateTimeText)
	if err != nil {
		log.Error("master: ", err)
		return true
	}
	return time.Since(updateTime).Minutes() > float64(timeLimit)
}

// CollectPopItem updates popular items for the database.
func (m *Master) CollectPopItem(items []data.Item, feedback []data.Feedback) {
	if m.cfg.Popular.NumPopular > 0 {
		log.Info("master: collect popular items")
		// create item mapping
		itemMap := make(map[string]data.Item)
		for _, item := range items {
			itemMap[item.ItemId] = item
		}
		// count feedback
		timeWindowLimit := time.Now().AddDate(0, 0, -m.cfg.Popular.TimeWindow)
		count := make(map[string]int)
		for _, fb := range feedback {
			if fb.Timestamp.After(timeWindowLimit) {
				count[fb.ItemId]++
			}
		}
		// collect pop items
		popItems := make(map[string]*base.TopKStringFilter)
		popItems[""] = base.NewTopKStringFilter(m.cfg.Popular.NumPopular)
		for itemId, f := range count {
			popItems[""].Push(itemId, float32(f))
			item := itemMap[itemId]
			for _, label := range item.Labels {
				if _, exists := popItems[label]; !exists {
					popItems[label] = base.NewTopKStringFilter(m.cfg.Popular.NumPopular)
				}
				popItems[label].Push(itemId, float32(f))
			}
		}
		// write back
		for label, topItems := range popItems {
			result, _ := topItems.PopAll()
			if err := m.cacheStore.SetList(cache.PopularItems, label, result); err != nil {
				log.Errorf("master: failed to cache popular items (%v)", err)
			}
		}
		if err := m.cacheStore.SetString(cache.GlobalMeta, cache.CollectPopularTime, base.Now()); err != nil {
			log.Errorf("master: failed to cache popular items (%v)", err)
		}
	}
}

// CollectLatest updates latest items.
func (m *Master) CollectLatest(items []data.Item) {
	if m.cfg.Latest.NumLatest > 0 {
		log.Info("master: collect latest items")
		var err error
		latestItems := make(map[string]*base.TopKStringFilter)
		latestItems[""] = base.NewTopKStringFilter(m.cfg.Latest.NumLatest)
		// find latest items
		for _, item := range items {
			latestItems[""].Push(item.ItemId, float32(item.Timestamp.Unix()))
			for _, label := range item.Labels {
				if _, exist := latestItems[label]; !exist {
					latestItems[label] = base.NewTopKStringFilter(m.cfg.Latest.NumLatest)
				}
				latestItems[label].Push(item.ItemId, float32(item.Timestamp.Unix()))
			}
		}
		for label, topItems := range latestItems {
			result, _ := topItems.PopAll()
			if err = m.cacheStore.SetList(cache.LatestItems, label, result); err != nil {
				log.Errorf("master: failed to cache latest items (%v)", err)
			}
		}
		if err = m.cacheStore.SetString(cache.GlobalMeta, cache.CollectLatestTime, base.Now()); err != nil {
			log.Errorf("master: failed to cache latest items (%v)", err)
		}
	}
}

// CollectSimilar updates neighbors for the database.
func (m *Master) CollectSimilar(items []data.Item, dataset *cf.DataSet) {
	if m.cfg.Similar.NumSimilar > 0 {
		log.Info("master: collect similar items")
		// create progress tracker
		completed := make(chan []interface{}, 1000)
		go func() {
			completedCount := 0
			ticker := time.NewTicker(time.Second)
			for {
				select {
				case _, ok := <-completed:
					if !ok {
						return
					}
					completedCount++
				case <-ticker.C:
					log.Infof("master: collect similar items (%v/%v)", completedCount, dataset.ItemCount())
				}
			}
		}()
		if err := base.Parallel(dataset.ItemCount(), m.cfg.Master.Jobs, func(workerId, jobId int) error {
			users := dataset.ItemFeedback[jobId]
			// Collect candidates
			itemSet := base.NewSet()
			for _, u := range users {
				itemSet.Add(dataset.UserFeedback[u]...)
			}
			// Ranking
			nearItems := base.NewTopKFilter(m.cfg.Similar.NumSimilar)
			for j := range itemSet {
				if j != jobId {
					nearItems.Push(j, Dot(dataset.ItemFeedback[jobId], dataset.ItemFeedback[j]))
				}
			}
			elem, _ := nearItems.PopAll()
			recommends := make([]string, len(elem))
			for i := range recommends {
				recommends[i] = dataset.ItemIndex.ToName(elem[i])
			}
			if err := m.cacheStore.SetList(cache.SimilarItems, dataset.ItemIndex.ToName(jobId), recommends); err != nil {
				return err
			}
			completed <- nil
			return nil
		}); err != nil {
			log.Errorf("master: failed to cache similar items (%v)", err)
		}
		close(completed)
		if err := m.cacheStore.SetString(cache.GlobalMeta, cache.CollectSimilarTime, base.Now()); err != nil {
			log.Errorf("master: failed to cache similar items (%v)", err)
		}
	}
}

func Dot(a, b []int) float32 {
	interSet := base.NewSet(a...)
	intersect := float32(0.0)
	for _, i := range b {
		if interSet.Contain(i) {
			intersect++
		}
	}
	if intersect == 0 {
		return 0
	}
	return intersect // math32.Sqrt(float32(len(a))) / math32.Sqrt(float32(len(b)))
}
