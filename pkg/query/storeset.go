package query

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/improbable-eng/thanos/pkg/store"
	"github.com/improbable-eng/thanos/pkg/store/storepb"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/tsdb/labels"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
)

type StoreSpec interface {
	// Address for the store spec. It is used as ID for store.
	Addr() string
	// Metadata returns current labels and min, max ranges for store.
	// It can change for every call for this method.
	// If metadata call fails we assume that store is no longer accessible and we should not use it.
	// NOTE: It is implementation responsibility to retry until context timeout, but a caller responsibilty to manage
	// given store connection.
	Metadata(ctx context.Context, client storepb.StoreClient) (labels []storepb.Label, mint int64, maxt int64, err error)
}

type staticStoreSpec struct {
	addr string
}

// NewStaticStoreSpec creates store spec for static store.
// This is used only for query command but we include this here to properly test this spec inside storeset_test.go
func NewStaticStoreSpec(addr string) StoreSpec {
	return &staticStoreSpec{addr: addr}
}

func (s *staticStoreSpec) Addr() string {
	// API addr should not change between state changes.
	return s.addr
}

// Metadata method for static store tries to reach host Info method until context timeout. If we are unable to get metadata after
// that time, we assume that the host is unhealthy and return error.
func (s *staticStoreSpec) Metadata(ctx context.Context, client storepb.StoreClient) (labels []storepb.Label, mint int64, maxt int64, err error) {
	resp, err := client.Info(ctx, &storepb.InfoRequest{}, grpc.FailFast(false))
	if err != nil {
		return nil, 0, 0, errors.Wrapf(err, "fetching store info from %s", s.addr)
	}
	return resp.Labels, resp.MinTime, resp.MaxTime, nil
}

// StoreSet maintains a set of active stores. It is backed up by Store Specifications that are dynamically fetched on
// every Update() call.
type StoreSet struct {
	logger log.Logger

	// Store specifications can change dynamically. If some store is missing from the list, we assuming it is no longer
	// accessible and we close gRPC client for it.
	storeSpecs       func() []StoreSpec
	dialOpts         []grpc.DialOption
	gRPCRetryTimeout time.Duration

	mtx                  sync.RWMutex
	stores               map[string]*storeRef
	storeNodeConnections prometheus.Gauge
	externalLabelStores  map[string]int
}

type storeSetNodeCollector struct {
	externalLabelOccurrences func() map[string]int
}

var (
	nodeInfoDesc = prometheus.NewDesc(
		"thanos_store_node_info",
		"Number of nodes with the same external labels identified by their hash. If any time-series is larger than 1, external label uniqueness is not true",
		[]string{"external_labels"}, nil,
	)
)

func (c *storeSetNodeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nodeInfoDesc
}

func (c *storeSetNodeCollector) Collect(ch chan<- prometheus.Metric) {
	externalLabelOccurrences := c.externalLabelOccurrences()
	for externalLabels, occurrences := range externalLabelOccurrences {
		ch <- prometheus.MustNewConstMetric(nodeInfoDesc, prometheus.GaugeValue, float64(occurrences), externalLabels)
	}
}

// NewStoreSet returns a new set of stores from cluster peers and statically configured ones.
func NewStoreSet(
	logger log.Logger,
	reg *prometheus.Registry,
	storeSpecs func() []StoreSpec,
	dialOpts []grpc.DialOption,
) *StoreSet {
	storeNodeConnections := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "thanos_store_nodes_grpc_connections",
		Help: "Number indicating current number of gRPC connection to store nodes. This indicates also to how many stores query node have access to.",
	})

	if logger == nil {
		logger = log.NewNopLogger()
	}
	if reg != nil {
		reg.MustRegister(storeNodeConnections)
	}
	if storeSpecs == nil {
		storeSpecs = func() []StoreSpec { return nil }
	}

	ss := &StoreSet{
		logger:               logger,
		storeSpecs:           storeSpecs,
		dialOpts:             dialOpts,
		storeNodeConnections: storeNodeConnections,
		gRPCRetryTimeout:     3 * time.Second,
		externalLabelStores:  map[string]int{},
	}

	storeNodeCollector := &storeSetNodeCollector{externalLabelOccurrences: ss.externalLabelOccurrences}
	if reg != nil {
		reg.MustRegister(storeNodeCollector)
	}

	return ss
}

type storeRef struct {
	storepb.StoreClient

	mtx  sync.RWMutex
	cc   *grpc.ClientConn
	addr string

	// Meta (can change during runtime).
	labels  []storepb.Label
	minTime int64
	maxTime int64
}

func (s *storeRef) Labels() []storepb.Label {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	return s.labels
}

func (s *storeRef) TimeRange() (int64, int64) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	return s.minTime, s.maxTime
}

func (s *storeRef) String() string {
	return fmt.Sprintf("%s", s.addr)
}

func (s *storeRef) close() {
	s.cc.Close()
}

func (s *StoreSet) updateStore(ctx context.Context, spec StoreSpec) (*storeRef, error) {
	ctx, cancel := context.WithTimeout(ctx, s.gRPCRetryTimeout)
	defer cancel()

	addr := spec.Addr()

	s.mtx.RLock()
	st, ok := s.stores[addr]
	s.mtx.RUnlock()
	if !ok {
		// New store or was unhealthy and was removed in the past - create new one.
		conn, err := grpc.DialContext(ctx, addr, s.dialOpts...)
		if err != nil {
			return nil, errors.Wrap(err, "dialing connection")
		}

		st = &storeRef{
			StoreClient: storepb.NewStoreClient(conn),
			cc:          conn,
			addr:        addr,
		}
	}

	var err error
	st.labels, st.minTime, st.maxTime, err = spec.Metadata(ctx, st.StoreClient)
	if err != nil {
		st.close()
		return nil, err
	}
	return st, nil
}

// Update updates the store set. It fetches current list of store specs from function and grabs fresh metadata.
func (s *StoreSet) Update(ctx context.Context) {
	var (
		stores   = make(map[string]*storeRef, len(s.stores))
		innerMtx sync.Mutex
		g        errgroup.Group
	)

	for _, storeSpec := range s.storeSpecs() {
		spec := storeSpec
		g.Go(func() error {
			addr := spec.Addr()
			st, err := s.updateStore(ctx, spec)
			if err != nil {
				return err
			}
			innerMtx.Lock()
			if dupSt, ok := stores[addr]; ok {
				level.Error(s.logger).Log("msg", "duplicated address in gossip or static store nodes.", "addr", addr)
				dupSt.close()
			}
			stores[addr] = st
			innerMtx.Unlock()

			return nil
		})
	}

	err := g.Wait()
	if err != nil {
		level.Warn(s.logger).Log("msg", "update of some store nodes failed", "err", err)
	}

	// Record the number of occurrences of external label combinations for current store slice.
	externalLabelStores := map[string]int{}
	for _, st := range stores {
		externalLabelStores[externalLabelsFromStore(st)]++
	}

	s.mtx.Lock()
	defer s.mtx.Unlock()

	// Remove stores that where not updated in this update, because were not in s.peerAddr() this time.
	for addr, st := range s.stores {
		if _, ok := stores[addr]; ok {
			continue
		}

		// Peer does not exists anymore.
		st.close()
	}

	// Swap old store set with new one, excluding these with duplicated external labels.
	s.stores = make(map[string]*storeRef, len(stores))
	for addr, st := range stores {
		// Check if it has some ext labels specified.
		// No external labels means strictly store gateway or ruler and it is fine to have access to multiple instances of them.
		//
		// Sidecar will error out if it will be configured with empty external labels.
		if len(st.Labels()) == 0 || externalLabelStores[externalLabelsFromStore(st)] == 1 {
			s.stores[addr] = st
			continue
		}

		level.Warn(s.logger).Log("msg", "dropping store, external labels are not unique", "address", addr)

		st.close()
	}

	s.externalLabelStores = externalLabelStores
	s.storeNodeConnections.Set(float64(len(s.stores)))
}

func externalLabelsFromStore(st *storeRef) string {
	tsdbLabels := labels.Labels{}
	for _, l := range st.labels {
		tsdbLabels = append(tsdbLabels, labels.Label{
			Name:  l.Name,
			Value: l.Value,
		})
	}
	sort.Sort(tsdbLabels)

	return tsdbLabels.String()
}

func (s *StoreSet) externalLabelOccurrences() map[string]int {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	r := make(map[string]int, len(s.externalLabelStores))
	for k, v := range s.externalLabelStores {
		r[k] = v
	}

	return r
}

// Get returns a list of all active stores.
func (s *StoreSet) Get() []store.Client {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	stores := make([]store.Client, 0, len(s.stores))
	for _, st := range s.stores {
		stores = append(stores, st)
	}
	return stores
}

func (s *StoreSet) Close() {
	for _, st := range s.stores {
		st.close()
	}
}
