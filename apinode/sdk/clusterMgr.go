package sdk

import (
	"context"
	"sync"
)

type ClusterManager interface {
	// AddCluster masterAddr, eg: host1:port,host2:port
	AddCluster(ctx context.Context, clusterId string, masterAddr string) error
	// GetCluster if not exist, return nil
	GetCluster(clusterId string) ICluster
}

type clusterMgr struct {
	clk        sync.RWMutex
	clusterMap map[string]ICluster

	create func(cId, addr string) (ICluster, error)
}

func NewClusterMgr() ClusterManager {
	cm := &clusterMgr{
		clusterMap: make(map[string]ICluster),
	}

	cm.create = newCluster
	return cm
}

func (cm *clusterMgr) getCluster(cId string) ICluster {
	cm.clk.RLock()
	defer cm.clk.RUnlock()

	return cm.clusterMap[cId]
}

func (cm *clusterMgr) putCluster(cId string, newC ICluster) {
	cm.clk.Lock()
	defer cm.clk.Unlock()

	cm.clusterMap[cId] = newC
}

func (cm *clusterMgr) AddCluster(ctx context.Context, cId string, masterAddr string) error {
	// check if cluster exist
	c := cm.getCluster(cId)
	if c != nil {
		if c.addr() == masterAddr {
			return nil
		}
		// update masterAddr
		return c.updateAddr(masterAddr)
	}

	c, err := cm.create(cId, masterAddr)
	if err != nil {
		return err
	}

	cm.putCluster(cId, c)
	return nil
}

func (cm *clusterMgr) GetCluster(cId string) ICluster {
	return cm.getCluster(cId)
}
