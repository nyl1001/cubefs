package metanode

import (
	"strings"
	"sync/atomic"
	"time"

	"github.com/chubaofs/chubaofs/util/log"
	"golang.org/x/time/rate"
)

const (
	UpdateNodeInfoTicket     = 1 * time.Minute
	UpdateClusterViewTicket  = 24 * time.Hour
	DefaultDeleteBatchCounts = 128
	DefaultReqLimitBurst     = 512
)

type NodeInfo struct {
	deleteBatchCount uint64
}

var (
	nodeInfo                   = &NodeInfo{}
	nodeInfoStopC              = make(chan struct{}, 0)
	deleteWorkerSleepMs uint64 = 0
	reqLimitRater              = rate.NewLimiter(rate.Inf, DefaultReqLimitBurst)
	clusterMap                 = make(map[string]bool)
)

func DeleteBatchCount() uint64 {
	val := atomic.LoadUint64(&nodeInfo.deleteBatchCount)
	if val == 0 {
		val = DefaultDeleteBatchCounts
	}
	return val
}

func updateDeleteBatchCount(val uint64) {
	atomic.StoreUint64(&nodeInfo.deleteBatchCount, val)
}

func updateDeleteWorkerSleepMs(val uint64) {
	atomic.StoreUint64(&deleteWorkerSleepMs, val)
}

func DeleteWorkerSleepMs() {
	val := atomic.LoadUint64(&deleteWorkerSleepMs)
	if val > 0 {
		time.Sleep(time.Duration(val) * time.Millisecond)
	}
}

func (m *MetaNode) startUpdateNodeInfo() {
	ticker := time.NewTicker(UpdateNodeInfoTicket)
	// call once on init before first tick
	m.updateClusterMap()
	clusterViewTicker := time.NewTicker(UpdateClusterViewTicket)
	defer ticker.Stop()
	defer clusterViewTicker.Stop()
	for {
		select {
		case <-nodeInfoStopC:
			log.LogInfo("metanode nodeinfo gorutine stopped")
			return
		case <-ticker.C:
			m.updateNodeInfo()
		case <-clusterViewTicker.C:
			m.updateClusterMap()
		}
	}
}

func (m *MetaNode) stopUpdateNodeInfo() {
	nodeInfoStopC <- struct{}{}
}

func (m *MetaNode) updateNodeInfo() {
	//clusterInfo, err := getClusterInfo()
	clusterInfo, err := masterClient.AdminAPI().GetClusterInfo()
	if err != nil {
		log.LogErrorf("[updateNodeInfo] %s", err.Error())
		return
	}
	updateDeleteBatchCount(clusterInfo.MetaNodeDeleteBatchCount)
	updateDeleteWorkerSleepMs(clusterInfo.MetaNodeDeleteWorkerSleepMs)
	r := clusterInfo.MetaNodeReqLimitRate
	l := rate.Limit(r)
	if r == 0 {
		l = rate.Inf
	}
	reqLimitRater.SetLimit(l)
}

func (m *MetaNode) updateClusterMap() {
	cv, err := masterClient.AdminAPI().GetCluster()
	if err != nil {
		return
	}
	addrMap := make(map[string]bool, len(clusterMap))
	var addrSlice []string
	for _, node := range cv.MetaNodes {
		addrSlice = strings.Split(node.Addr, ":")
		addrMap[addrSlice[0]] = true
	}
	for _, node := range cv.DataNodes {
		addrSlice = strings.Split(node.Addr, ":")
		addrMap[addrSlice[0]] = true
	}
	for _, master := range masterClient.Nodes() {
		addrSlice = strings.Split(master, ":")
		addrMap[addrSlice[0]] = true
	}
	clusterMap = addrMap
}
