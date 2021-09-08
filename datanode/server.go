// Copyright 2018 The Chubao Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package datanode

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chubaofs/chubaofs/cmd/common"
	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/raftstore"
	"github.com/chubaofs/chubaofs/repl"
	masterSDK "github.com/chubaofs/chubaofs/sdk/master"
	"github.com/chubaofs/chubaofs/util"
	"github.com/chubaofs/chubaofs/util/config"
	"github.com/chubaofs/chubaofs/util/exporter"
	"github.com/chubaofs/chubaofs/util/log"
	"github.com/chubaofs/chubaofs/util/statistics"
)

var (
	ErrIncorrectStoreType       = errors.New("Incorrect store type")
	ErrNoSpaceToCreatePartition = errors.New("No disk space to create a data partition")
	ErrNewSpaceManagerFailed    = errors.New("Creater new space manager failed")

	LocalIP, serverPort   string
	gConnPool             = util.NewConnectPool()
	MasterClient          = masterSDK.NewMasterClient(nil, false)
	gHasLoadDataPartition bool
)

const (
	DefaultZoneName          = proto.DefaultZoneName
	DefaultRaftDir           = "raft"
	DefaultRaftLogsToRetain  = 10 // Count of raft logs per data partition
	DefaultDiskMaxErr        = 1
	DefaultDiskReservedSpace = 5 * util.GB // GB
	DefaultDiskReservedRatio = float64(0.1)
	DefaultDiskRetainMax     = 30 * util.GB // GB
)

const (
	ModuleName = "dataNode"
)

const (
	ConfigKeyLocalIP       = "localIP"        // string
	ConfigKeyPort          = "port"           // int
	ConfigKeyMasterAddr    = "masterAddr"     // array
	ConfigKeyZone          = "zoneName"       // string
	ConfigKeyDisks         = "disks"          // array
	ConfigKeyRaftDir       = "raftDir"        // string
	ConfigKeyRaftHeartbeat = "raftHeartbeat"  // string
	ConfigKeyRaftReplica   = "raftReplica"    // string
	cfgTickIntervalMs      = "tickIntervalMs" // int
)

// DataNode defines the structure of a data node.
type DataNode struct {
	space                    *SpaceManager
	port                     string
	zoneName                 string
	clusterID                string
	localIP                  string
	localServerAddr          string
	nodeID                   uint64
	raftDir                  string
	raftHeartbeat            string
	raftReplica              string
	raftStore                raftstore.RaftStore
	tickInterval             int
	tcpListener              net.Listener
	stopC                    chan bool
	fixTinyDeleteRecordLimit uint64
	control                  common.Control
}

func NewServer() *DataNode {
	return &DataNode{}
}

func (s *DataNode) Start(cfg *config.Config) (err error) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	return s.control.Start(s, cfg, doStart)
}

// Shutdown shuts down the current data node.
func (s *DataNode) Shutdown() {
	s.control.Shutdown(s, doShutdown)
}

// Sync keeps data node in sync.
func (s *DataNode) Sync() {
	s.control.Sync()
}

// Workflow of starting up a data node.
func doStart(server common.Server, cfg *config.Config) (err error) {
	s, ok := server.(*DataNode)
	if !ok {
		return errors.New("Invalid Node Type!")
	}

	s.stopC = make(chan bool, 0)

	// parse the config file
	if err = s.parseConfig(cfg); err != nil {
		return
	}
	log.LogErrorf("doStart parseConfig fininsh")
	s.register(cfg)
	log.LogErrorf("doStart register fininsh")
	exporter.Init(s.clusterID, ModuleName, cfg)
	exporter.RegistConsul(cfg)

	// start the raft server
	if err = s.startRaftServer(cfg); err != nil {
		return
	}
	log.LogErrorf("doStart startRaftServer fininsh")

	// create space manager (disk, partition, etc.)
	if err = s.startSpaceManager(cfg); err != nil {
		return
	}
	log.LogErrorf("doStart startSpaceManager fininsh")

	// check local partition compare with master ,if lack,then not start
	if err = s.checkLocalPartitionMatchWithMaster(); err != nil {
		fmt.Println(err)
		exporter.Warning(err.Error())
		return
	}
	log.LogErrorf("doStart checkLocalPartitionMatchWithMaster fininsh")

	// start tcp listening
	if err = s.startTCPService(); err != nil {
		return
	}

	log.LogErrorf("doStart startTCPService fininsh")

	// Start all loaded data partitions which managed by space manager,
	// this operation will start raft partitions belong to data partitions.
	s.space.StartPartitions()
	log.LogErrorf("doStart start dataPartition raft  fininsh")
	gHasLoadDataPartition = true

	go s.registerHandler()

	go s.startUpdateNodeInfo()

	statistics.InitStatistics(cfg, s.clusterID, statistics.ModelDataNode, LocalIP, s.summaryMonitorData)

	return
}

func doShutdown(server common.Server) {
	s, ok := server.(*DataNode)
	if !ok {
		return
	}
	close(s.stopC)
	s.space.Stop()
	s.stopUpdateNodeInfo()
	s.stopTCPService()
	s.stopRaftServer()
}

func (s *DataNode) parseConfig(cfg *config.Config) (err error) {
	var (
		port       string
		regexpPort *regexp.Regexp
	)
	LocalIP = cfg.GetString(ConfigKeyLocalIP)
	port = cfg.GetString(proto.ListenPort)
	serverPort = port
	if regexpPort, err = regexp.Compile("^(\\d)+$"); err != nil {
		return fmt.Errorf("Err:no port")
	}
	if !regexpPort.MatchString(port) {
		return fmt.Errorf("Err:port must string")
	}
	s.port = port
	if len(cfg.GetSlice(proto.MasterAddr)) == 0 {
		return fmt.Errorf("Err:masterAddr unavalid")
	}
	for _, ip := range cfg.GetSlice(proto.MasterAddr) {
		MasterClient.AddNode(ip.(string))
	}
	s.zoneName = cfg.GetString(ConfigKeyZone)
	if s.zoneName == "" {
		s.zoneName = DefaultZoneName
	}

	s.tickInterval = int(cfg.GetFloat(cfgTickIntervalMs))
	if s.tickInterval <= 300 {
		log.LogWarnf("get config [%s]:[%v] less than 300 so set it to 500 ", cfgTickIntervalMs, cfg.GetString(cfgTickIntervalMs))
		s.tickInterval = 500
	}

	log.LogDebugf("action[parseConfig] load masterAddrs(%v).", MasterClient.Nodes())
	log.LogDebugf("action[parseConfig] load port(%v).", s.port)
	log.LogDebugf("action[parseConfig] load zoneName(%v).", s.zoneName)
	return
}

func (s *DataNode) startSpaceManager(cfg *config.Config) (err error) {
	s.space = NewSpaceManager(s)
	if len(strings.TrimSpace(s.port)) == 0 {
		err = ErrNewSpaceManagerFailed
		return
	}

	s.space.SetRaftStore(s.raftStore)
	s.space.SetNodeID(s.nodeID)
	s.space.SetClusterID(s.clusterID)

	var wg sync.WaitGroup
	for _, d := range cfg.GetSlice(ConfigKeyDisks) {
		log.LogDebugf("action[startSpaceManager] load disk raw config(%v).", d)

		// format "PATH:RESET_SIZE
		arr := strings.Split(d.(string), ":")
		if len(arr) == 0 {
			return errors.New("Invalid disk configuration. Example: PATH[:RESERVE_SIZE]")
		}
		path := arr[0]
		fileInfo, err := os.Stat(path)
		if err != nil {
			return errors.New(fmt.Sprintf("Stat disk path error: %s", err.Error()))
		}
		if !fileInfo.IsDir() {
			return errors.New("Disk path is not dir")
		}

		wg.Add(1)
		go func(wg *sync.WaitGroup, path string) {
			defer wg.Done()
			var (
				reserved uint64 = DefaultDiskReservedSpace
				stateFS         = new(syscall.Statfs_t)
			)
			if err := syscall.Statfs(path, stateFS); err == nil {
				reserved = uint64(float64(stateFS.Blocks*uint64(stateFS.Bsize)) * DefaultDiskReservedRatio)
			}
			log.LogInfof("disk [%v] load with option [ReservedSpace: %v, MaxErrorCount: %v",
				path, reserved, DefaultDiskMaxErr)
			s.space.LoadDisk(path, reserved, DefaultDiskMaxErr)
		}(&wg, path)
	}
	wg.Wait()
	return nil
}

// registers the data node on the master to report the information such as IsIPV4 address.
// The startup of a data node will be blocked until the registration succeeds.
func (s *DataNode) register(cfg *config.Config) {
	var (
		err error
	)

	timer := time.NewTimer(0)

	// get the IsIPV4 address, cluster ID and node ID from the master
	for {
		select {
		case <-timer.C:
			var ci *proto.ClusterInfo
			if ci, err = MasterClient.AdminAPI().GetClusterInfo(); err != nil {
				log.LogErrorf("action[registerToMaster] cannot get ip from master(%v) err(%v).",
					MasterClient.Leader(), err)
				timer.Reset(2 * time.Second)
				continue
			}
			masterAddr := MasterClient.Leader()
			s.clusterID = ci.Cluster
			if LocalIP == "" {
				LocalIP = string(ci.Ip)
			}
			s.localServerAddr = fmt.Sprintf("%s:%v", LocalIP, s.port)
			if !util.IsIPV4(LocalIP) {
				log.LogErrorf("action[registerToMaster] got an invalid local ip(%v) from master(%v).",
					LocalIP, masterAddr)
				timer.Reset(2 * time.Second)
				continue
			}

			// register this data node on the master
			var nodeID uint64
			if nodeID, err = MasterClient.NodeAPI().AddDataNode(fmt.Sprintf("%s:%v", LocalIP, s.port), s.zoneName); err != nil {
				log.LogErrorf("action[registerToMaster] cannot register this node to master[%v] err(%v).",
					masterAddr, err)
				timer.Reset(2 * time.Second)
				continue
			}
			s.nodeID = nodeID
			log.LogDebugf("register: register DataNode: nodeID(%v)", s.nodeID)
			return
		case <-s.stopC:
			timer.Stop()
			return
		}
	}
}

type DataNodeInfo struct {
	Addr                      string
	PersistenceDataPartitions []uint64
}

func (s *DataNode) checkLocalPartitionMatchWithMaster() (err error) {
	var convert = func(node *proto.DataNodeInfo) *DataNodeInfo {
		result := &DataNodeInfo{}
		result.Addr = node.Addr
		result.PersistenceDataPartitions = node.PersistenceDataPartitions
		return result
	}
	var dataNode *proto.DataNodeInfo
	for i := 0; i < 3; i++ {
		if dataNode, err = MasterClient.NodeAPI().GetDataNode(s.localServerAddr); err != nil {
			log.LogErrorf("checkLocalPartitionMatchWithMaster error %v", err)
			continue
		}
		break
	}
	dinfo := convert(dataNode)
	if len(dinfo.PersistenceDataPartitions) == 0 {
		return
	}
	lackPartitions := make([]uint64, 0)
	for _, partitionID := range dinfo.PersistenceDataPartitions {
		dp := s.space.Partition(partitionID)
		if dp == nil {
			lackPartitions = append(lackPartitions, partitionID)
		}
	}
	if len(lackPartitions) == 0 {
		return
	}
	err = fmt.Errorf("LackPartitions %v on datanode %v,datanode cannot start", lackPartitions, s.localServerAddr)
	log.LogErrorf(err.Error())
	return
}

func (s *DataNode) registerHandler() {
	http.HandleFunc(proto.VersionPath, func(w http.ResponseWriter, _ *http.Request) {
		version := proto.MakeVersion("DataNode")
		marshal, _ := json.Marshal(version)
		if _, err := w.Write(marshal); err != nil {
			log.LogErrorf("write version has err:[%s]", err.Error())
		}
	})
	http.HandleFunc("/disks", s.getDiskAPI)
	http.HandleFunc("/partitions", s.getPartitionsAPI)
	http.HandleFunc("/partition", s.getPartitionAPI)
	http.HandleFunc("/extent", s.getExtentAPI)
	http.HandleFunc("/block", s.getBlockCrcAPI)
	http.HandleFunc("/stats", s.getStatAPI)
	http.HandleFunc("/raftStatus", s.getRaftStatus)
	http.HandleFunc("/setAutoRepairStatus", s.setAutoRepairStatus)
	http.HandleFunc("/releasePartitions", s.releasePartitions)
	http.HandleFunc("/computeExtentMd5", s.getExtentMd5Sum)
}

func (s *DataNode) startTCPService() (err error) {
	log.LogInfo("Start: startTCPService")
	addr := fmt.Sprintf(":%v", s.port)
	l, err := net.Listen(NetworkProtocol, addr)
	log.LogDebugf("action[startTCPService] listen %v address(%v).", NetworkProtocol, addr)
	if err != nil {
		log.LogError("failed to listen, err:", err)
		return
	}
	s.tcpListener = l
	go func(ln net.Listener) {
		for {
			conn, err := ln.Accept()
			if err != nil {
				log.LogErrorf("action[startTCPService] failed to accept, err:%s", err.Error())
				break
			}
			log.LogDebugf("action[startTCPService] accept connection from %s.", conn.RemoteAddr().String())
			go s.serveConn(conn)
		}
	}(l)
	return
}

func (s *DataNode) stopTCPService() (err error) {
	if s.tcpListener != nil {

		s.tcpListener.Close()
		log.LogDebugf("action[stopTCPService] stop tcp service.")
	}
	return
}

func (s *DataNode) serveConn(conn net.Conn) {
	space := s.space
	space.Stats().AddConnection()
	c, _ := conn.(*net.TCPConn)
	c.SetKeepAlive(true)
	c.SetNoDelay(true)
	packetProcessor := repl.NewReplProtocol(c, s.Prepare, s.OperatePacket, s.Post)
	packetProcessor.ServerConn()
}

// Increase the disk error count by one.
func (s *DataNode) incDiskErrCnt(partitionID uint64, err error, flag uint8) {
	if err == nil {
		return
	}
	dp := s.space.Partition(partitionID)
	if dp == nil {
		return
	}
	d := dp.Disk()
	if d == nil {
		return
	}
	if !IsDiskErr(err.Error()) {
		return
	}
	if flag == WriteFlag {
		d.incWriteErrCnt()
	} else if flag == ReadFlag {
		d.incReadErrCnt()
	}
}

func IsDiskErr(errMsg string) bool {
	if strings.Contains(errMsg, syscall.EIO.Error()) || strings.Contains(errMsg, syscall.EROFS.Error()) {
		return true
	}

	return false
}

func (s *DataNode) summaryMonitorData(reportTime int64) []*statistics.MonitorData {
	dataList := make([]*statistics.MonitorData, 0)
	s.space.RangePartitions(func(partition *DataPartition) bool {
		totalCount := uint64(0)
		// each op
		for i := 0; i < len(partition.monitorData); i++ {
			if atomic.LoadUint64(&partition.monitorData[i].Count) == 0 {
				continue
			}
			data := &statistics.MonitorData{
				VolName:     partition.volumeID,
				PartitionID: partition.partitionID,
				Action:      i,
				ActionStr:   statistics.ActionDataMap[i],
				Size:        atomic.SwapUint64(&partition.monitorData[i].Size, 0),
				Count:       atomic.SwapUint64(&partition.monitorData[i].Count, 0),
				ReportTime:  reportTime,
			}
			dataList = append(dataList, data)
			totalCount += data.Count
		}
		// total count
		if totalCount > 0 {
			totalData := &statistics.MonitorData{
				VolName:     partition.volumeID,
				PartitionID: partition.partitionID,
				Count:       totalCount,
				ReportTime:  reportTime,
				IsTotal:     true,
			}
			dataList = append(dataList, totalData)
		}
		return true
	})
	return dataList
}
