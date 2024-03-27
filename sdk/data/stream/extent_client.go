// Copyright 2018 The CubeFS Authors.
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

package stream

import (
	"container/list"
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/data/wrapper"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/stat"

	"golang.org/x/time/rate"
)

type (
	SplitExtentKeyFunc  func(ctx context.Context, parentInode, inode uint64, key proto.ExtentKey) error
	AppendExtentKeyFunc func(ctx context.Context, parentInode, inode uint64, key proto.ExtentKey, discard []proto.ExtentKey) (int, error)
	GetExtentsFunc      func(ctx context.Context, inode uint64) (uint64, uint64, []proto.ExtentKey, error)
	TruncateFunc        func(ctx context.Context, inode, size uint64, fullPath string) error
	EvictIcacheFunc     func(inode uint64)
	LoadBcacheFunc      func(_ context.Context, key string, buf []byte, offset uint64, size uint32) (int, error)
	CacheBcacheFunc     func(_ context.Context, key string, buf []byte) error
	EvictBacheFunc      func(_ context.Context, key string) error
)

const (
	MaxMountRetryLimit = 6
	MountRetryInterval = time.Second * 5

	defaultReadLimitRate  = rate.Inf
	defaultReadLimitBurst = 128

	defaultWriteLimitRate  = rate.Inf
	defaultWriteLimitBurst = 128

	defaultStreamerLimit = 100000
	defMaxStreamerLimit  = 10000000
	kHighWatermarkPct    = 1.01
	slowStreamerEvictNum = 10
	fastStreamerEvictNum = 10000
)

var (
	// global object pools for memory optimization
	openRequestPool    *sync.Pool
	writeRequestPool   *sync.Pool
	flushRequestPool   *sync.Pool
	releaseRequestPool *sync.Pool
	truncRequestPool   *sync.Pool
	evictRequestPool   *sync.Pool
)

func init() {
	// init object pools
	openRequestPool = &sync.Pool{New: func() interface{} {
		return &OpenRequest{}
	}}
	writeRequestPool = &sync.Pool{New: func() interface{} {
		return &WriteRequest{}
	}}
	flushRequestPool = &sync.Pool{New: func() interface{} {
		return &FlushRequest{}
	}}
	releaseRequestPool = &sync.Pool{New: func() interface{} {
		return &ReleaseRequest{}
	}}
	truncRequestPool = &sync.Pool{New: func() interface{} {
		return &TruncRequest{}
	}}
	evictRequestPool = &sync.Pool{New: func() interface{} {
		return &EvictRequest{}
	}}
}

type ExtentConfig struct {
	Volume            string
	VolumeType        int
	Masters           []string
	FollowerRead      bool
	NearRead          bool
	Preload           bool
	ReadRate          int64
	WriteRate         int64
	BcacheEnable      bool
	BcacheDir         string
	MaxStreamerLimit  int64
	VerReadSeq        uint64
	OnAppendExtentKey AppendExtentKeyFunc
	OnSplitExtentKey  SplitExtentKeyFunc
	OnGetExtents      GetExtentsFunc
	OnTruncate        TruncateFunc
	OnEvictIcache     EvictIcacheFunc
	OnLoadBcache      LoadBcacheFunc
	OnCacheBcache     CacheBcacheFunc
	OnEvictBcache     EvictBacheFunc

	DisableMetaCache             bool
	MinWriteAbleDataPartitionCnt int
}

type MultiVerMgr struct {
	verReadSeq   uint64 // verSeq in config used as snapshot read
	latestVerSeq uint64 // newest verSeq from master for datanode write to check
	verList      *proto.VolVersionInfoList
	sync.RWMutex
}

// ExtentClient defines the struct of the extent client.
type ExtentClient struct {
	streamers          map[uint64]*Streamer
	streamerList       *list.List
	streamerLock       sync.Mutex
	maxStreamerLimit   int
	readLimiter        *rate.Limiter
	writeLimiter       *rate.Limiter
	disableMetaCache   bool
	volumeType         int
	volumeName         string
	bcacheEnable       bool
	bcacheDir          string
	BcacheHealth       bool
	preload            bool
	LimitManager       *manager.LimitManager
	dataWrapper        *wrapper.Wrapper
	appendExtentKey    AppendExtentKeyFunc
	splitExtentKey     SplitExtentKeyFunc
	getExtents         GetExtentsFunc
	truncate           TruncateFunc
	evictIcache        EvictIcacheFunc // May be null, must check before using
	loadBcache         LoadBcacheFunc
	cacheBcache        CacheBcacheFunc
	evictBcache        EvictBacheFunc
	inflightL1cache    sync.Map
	inflightL1BigBlock int32
	multiVerMgr        *MultiVerMgr
}

func (client *ExtentClient) UidIsLimited(ctx context.Context, uid uint32) bool {
	span := proto.SpanFromContext(ctx)
	client.dataWrapper.UidLock.RLock()
	defer client.dataWrapper.UidLock.RUnlock()
	if uInfo, ok := client.dataWrapper.Uids[uid]; ok {
		if uInfo.Limited {
			span.Debugf("uid %v is limited", uid)
			return true
		}
	}
	span.Debugf("uid %v is not limited", uid)
	return false
}

func (client *ExtentClient) evictStreamer() bool {
	// remove from list
	item := client.streamerList.Back()
	if item == nil {
		return false
	}

	client.streamerList.Remove(item)
	ino := item.Value.(uint64)

	s, ok := client.streamers[ino]
	if !ok {
		return true
	}

	if s.isOpen {
		client.streamerList.PushFront(ino)
		return true
	}

	delete(s.client.streamers, s.inode)
	return true
}

func (client *ExtentClient) batchEvictStramer(batchCnt int) {
	client.streamerLock.Lock()
	defer client.streamerLock.Unlock()

	for cnt := 0; cnt < batchCnt; cnt++ {
		ok := client.evictStreamer()
		if !ok {
			break
		}
	}
}

func (client *ExtentClient) backgroundEvictStream(ctx context.Context) {
	span := proto.SpanFromContext(ctx)
	t := time.NewTicker(2 * time.Second)
	for range t.C {
		start := time.Now()
		streamerSize := client.streamerList.Len()
		highWatermark := int(float32(client.maxStreamerLimit) * kHighWatermarkPct)
		for streamerSize > client.maxStreamerLimit {
			// fast evict
			if streamerSize > highWatermark {
				client.batchEvictStramer(fastStreamerEvictNum)
			} else {
				client.batchEvictStramer(slowStreamerEvictNum)
			}
			streamerSize = client.streamerList.Len()
			span.Infof("batch evict cnt(%d), cost(%d), now(%d)", 1, time.Since(start).Microseconds(), streamerSize)
		}
		span.Infof("streamer total cnt(%d), cost(%d) ns", streamerSize, time.Since(start).Nanoseconds())
	}
}

// NewExtentClient returns a new extent client.
func NewExtentClient(ctx context.Context, config *ExtentConfig) (client *ExtentClient, err error) {
	span := proto.SpanFromContext(ctx)
	client = new(ExtentClient)
	client.LimitManager = manager.NewLimitManager(ctx, client)
	client.LimitManager.WrapperUpdate = client.UploadFlowInfo
	limit := 0
retry:

	client.dataWrapper, err = wrapper.NewDataPartitionWrapper(ctx, client, config.Volume, config.Masters, config.Preload, config.MinWriteAbleDataPartitionCnt, config.VerReadSeq)
	if err != nil {
		span.Errorf("NewExtentClient: new data partition wrapper failed: volume(%v) mayRetry(%v) err(%v)",
			config.Volume, limit, err)
		if strings.Contains(err.Error(), proto.ErrVolNotExists.Error()) {
			return nil, proto.ErrVolNotExists
		}
		if limit >= MaxMountRetryLimit {
			return nil, errors.Trace(err, "Init data wrapper failed!")
		} else {
			limit++
			time.Sleep(MountRetryInterval * time.Duration(limit))
			goto retry
		}
	}

	client.streamers = make(map[uint64]*Streamer)
	client.multiVerMgr = &MultiVerMgr{verList: &proto.VolVersionInfoList{}}

	client.appendExtentKey = config.OnAppendExtentKey
	client.splitExtentKey = config.OnSplitExtentKey
	client.getExtents = config.OnGetExtents
	client.truncate = config.OnTruncate
	client.evictIcache = config.OnEvictIcache
	client.dataWrapper.InitFollowerRead(config.FollowerRead)
	client.dataWrapper.SetNearRead(ctx, config.NearRead)
	client.loadBcache = config.OnLoadBcache
	client.cacheBcache = config.OnCacheBcache
	client.evictBcache = config.OnEvictBcache
	client.volumeType = config.VolumeType
	client.volumeName = config.Volume
	client.bcacheEnable = config.BcacheEnable
	client.bcacheDir = config.BcacheDir
	client.multiVerMgr.verReadSeq = client.dataWrapper.GetReadVerSeq()
	client.BcacheHealth = true
	client.preload = config.Preload
	client.disableMetaCache = config.DisableMetaCache

	var readLimit, writeLimit rate.Limit
	if config.ReadRate <= 0 {
		readLimit = defaultReadLimitRate
	} else {
		readLimit = rate.Limit(config.ReadRate)
	}
	if config.WriteRate <= 0 {
		writeLimit = defaultWriteLimitRate
	} else {
		writeLimit = rate.Limit(config.WriteRate)
	}
	client.readLimiter = rate.NewLimiter(readLimit, defaultReadLimitBurst)
	client.writeLimiter = rate.NewLimiter(writeLimit, defaultWriteLimitBurst)

	if config.MaxStreamerLimit <= 0 {
		client.disableMetaCache = true
		return
	}

	if config.MaxStreamerLimit <= defaultStreamerLimit {
		client.maxStreamerLimit = defaultStreamerLimit
	} else if config.MaxStreamerLimit > defMaxStreamerLimit {
		client.maxStreamerLimit = defMaxStreamerLimit
	} else {
		client.maxStreamerLimit = int(config.MaxStreamerLimit)
	}

	client.maxStreamerLimit += fastStreamerEvictNum

	span.Infof("max streamer limit %d", client.maxStreamerLimit)
	client.streamerList = list.New()
	go client.backgroundEvictStream(ctx)

	return
}

func (client *ExtentClient) GetEnablePosixAcl() bool {
	return client.dataWrapper.EnablePosixAcl
}

func (client *ExtentClient) GetFlowInfo(ctx context.Context) (*proto.ClientReportLimitInfo, bool) {
	span := proto.SpanFromContext(ctx)
	span.Info("action[ExtentClient.GetFlowInfo]")
	return client.LimitManager.GetFlowInfo(ctx)
}

func (client *ExtentClient) UpdateFlowInfo(ctx context.Context, limit *proto.LimitRsp2Client) {
	span := proto.SpanFromContext(ctx)
	span.Infof("action[UpdateFlowInfo.UpdateFlowInfo]")
	client.LimitManager.SetClientLimit(ctx, limit)
}

func (client *ExtentClient) SetClientID(id uint64) (err error) {
	client.LimitManager.ID = id
	return
}

func (client *ExtentClient) GetVolumeName() string {
	return client.volumeName
}

func (client *ExtentClient) GetLatestVer() uint64 {
	return atomic.LoadUint64(&client.multiVerMgr.latestVerSeq)
}

func (client *ExtentClient) GetReadVer() uint64 {
	return atomic.LoadUint64(&client.multiVerMgr.verReadSeq)
}

func (client *ExtentClient) GetVerMgr() *proto.VolVersionInfoList {
	return client.multiVerMgr.verList
}

func (client *ExtentClient) UpdateLatestVer(ctx context.Context, verList *proto.VolVersionInfoList) (err error) {
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "UpdateLatestVer")
	verSeq := verList.GetLastVer()
	span.Debugf("action[UpdateLatestVer] verSeq %v verList[%v] mgr seq %v", verSeq, verList, client.multiVerMgr.latestVerSeq)
	if verSeq == 0 || verSeq <= atomic.LoadUint64(&client.multiVerMgr.latestVerSeq) {
		return
	}
	client.multiVerMgr.Lock()
	defer client.multiVerMgr.Unlock()
	if verSeq <= atomic.LoadUint64(&client.multiVerMgr.latestVerSeq) {
		return
	}

	span.Debugf("action[UpdateLatestVer] update verSeq [%v] to [%v]", client.multiVerMgr.latestVerSeq, verSeq)
	atomic.StoreUint64(&client.multiVerMgr.latestVerSeq, verSeq)
	client.multiVerMgr.verList = verList

	client.streamerLock.Lock()
	defer client.streamerLock.Unlock()
	for _, streamer := range client.streamers {
		if streamer.verSeq != verSeq {
			span.Debugf("action[ExtentClient.UpdateLatestVer] stream inode %v ver %v try update to %v", streamer.inode, streamer.verSeq, verSeq)
			oldVer := streamer.verSeq
			streamer.verSeq = verSeq
			streamer.extents.verSeq = verSeq
			if err = streamer.GetExtentsForce(ctx); err != nil {
				span.Errorf("action[UpdateLatestVer] inode %v streamer %v", streamer.inode, streamer.verSeq)
				streamer.verSeq = oldVer
				streamer.extents.verSeq = oldVer
				return err
			}
			atomic.StoreInt32(&streamer.needUpdateVer, 1)
			span.Debugf("action[ExtentClient.UpdateLatestVer] finhsed stream inode %v ver update to %v", streamer.inode, verSeq)
		}
	}
	return nil
}

// Open request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) OpenStream(ctx context.Context, inode uint64) error {
	client.streamerLock.Lock()
	s, ok := client.streamers[inode]
	if !ok {
		s = NewStreamer(ctx, client, inode)
		client.streamers[inode] = s
	}
	return s.IssueOpenRequest()
}

// Open request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) OpenStreamWithCache(ctx context.Context, inode uint64, needBCache bool) error {
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "OpenStreamWithCache")
	client.streamerLock.Lock()
	s, ok := client.streamers[inode]
	if !ok {
		s = NewStreamer(ctx, client, inode)
		client.streamers[inode] = s
		if !client.disableMetaCache && needBCache {
			client.streamerList.PushFront(inode)
		}
	}
	s.needBCache = needBCache
	if !s.isOpen && !client.disableMetaCache {
		s.isOpen = true
		span.Debugf("open stream again, ino(%v)", s.inode)
		s.request = make(chan interface{}, 64)
		s.pendingCache = make(chan bcacheKey, 1)
		go s.server(ctx)
		go s.asyncBlockCache()
	}
	return s.IssueOpenRequest()
}

// Release request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) CloseStream(inode uint64) error {
	client.streamerLock.Lock()
	s, ok := client.streamers[inode]
	if !ok {
		client.streamerLock.Unlock()
		return nil
	}
	return s.IssueReleaseRequest()
}

// Evict request shall grab the lock until request is sent to the request channel
func (client *ExtentClient) EvictStream(inode uint64) error {
	client.streamerLock.Lock()
	s, ok := client.streamers[inode]
	if !ok {
		client.streamerLock.Unlock()
		return nil
	}
	if s.isOpen {
		s.isOpen = false
		err := s.IssueEvictRequest()
		if err != nil {
			return err
		}
		s.done <- struct{}{}
	} else {
		delete(s.client.streamers, s.inode)
		s.client.streamerLock.Unlock()
	}

	return nil
}

// RefreshExtentsCache refreshes the extent cache.
func (client *ExtentClient) RefreshExtentsCache(ctx context.Context, inode uint64) error {
	ctx = proto.ContextWithOperation(ctx, "RefreshExtentsCache")
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		return nil
	}
	return s.GetExtents(ctx)
}

func (client *ExtentClient) ForceRefreshExtentsCache(ctx context.Context, inode uint64) error {
	ctx = proto.ContextWithOperation(ctx, "ForceRefreshExtentsCache")
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		return nil
	}
	return s.GetExtentsForce(ctx)
}

// GetExtentCacheGen return extent generation
func (client *ExtentClient) GetExtentCacheGen(ctx context.Context, inode uint64) uint64 {
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		return 0
	}
	return s.extents.gen
}

func (client *ExtentClient) GetExtents(ctx context.Context, inode uint64) []*proto.ExtentKey {
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		return nil
	}
	return s.extents.List()
}

// FileSize returns the file size.
func (client *ExtentClient) FileSize(ctx context.Context, inode uint64) (size int, gen uint64, valid bool) {
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		return
	}
	valid = true
	size, gen = s.extents.Size()
	return
}

// SetFileSize set the file size.
func (client *ExtentClient) SetFileSize(ctx context.Context, inode uint64, size int) {
	span := proto.SpanFromContext(ctx)
	s := client.GetStreamer(ctx, inode)
	if s != nil {
		span.Debugf("SetFileSize: ino(%v) size(%v)", inode, size)
		s.extents.SetSize(uint64(size), true)
	}
}

// Write writes the data.
func (client *ExtentClient) Write(ctx context.Context, inode uint64, offset int, data []byte, flags int, checkFunc func() error) (write int, err error) {
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "Write")
	prefix := fmt.Sprintf("Write{ino(%v)offset(%v)size(%v)}", inode, offset, len(data))
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		span.Errorf("Prefix(%v): stream is not opened yet", prefix)
		return 0, syscall.EBADF
	}

	s.once.Do(func() {
		// TODO unhandled error
		s.GetExtents(ctx)
	})

	write, err = s.IssueWriteRequest(offset, data, flags, checkFunc)
	if err != nil {
		span.Errorf(errors.Stack(err))
		exporter.Warning(err.Error())
	}
	return
}

func (client *ExtentClient) Truncate(ctx context.Context, mw *meta.MetaWrapper, parentIno uint64, inode uint64, size int, fullPath string) error {
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "Truncate")
	prefix := fmt.Sprintf("Truncate{ino(%v)size(%v)}", inode, size)
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		span.Errorf("Prefix(%v): stream is not opened yet", prefix)
		return syscall.EBADF
	}
	var info *proto.InodeInfo
	var err error
	var oldSize uint64
	if mw.EnableSummary {
		info, _ = mw.InodeGet_ll(ctx, inode)
		oldSize = info.Size
	}
	err = s.IssueTruncRequest(size, fullPath)
	if err != nil {
		err = errors.Trace(err, prefix)
		span.Errorf(errors.Stack(err))
	}
	if mw.EnableSummary {
		go mw.UpdateSummary_ll(ctx, parentIno, 0, 0, int64(size)-int64(oldSize))
	}

	return err
}

func (client *ExtentClient) Flush(ctx context.Context, inode uint64) error {
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "Flush")
	s := client.GetStreamer(ctx, inode)
	if s == nil {
		span.Errorf("Flush: stream is not opened yet, ino(%v)", inode)
		return syscall.EBADF
	}
	return s.IssueFlushRequest()
}

func (client *ExtentClient) Read(ctx context.Context, inode uint64, data []byte, offset int, size int) (read int, err error) {
	// log.Errorf("======> ExtentClient Read Enter, inode(%v), len(data)=(%v), offset(%v), size(%v).", inode, len(data), offset, size)
	// t1 := time.Now()
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "Read")

	if size == 0 {
		return
	}

	s := client.GetStreamer(ctx, inode)
	if s == nil {
		span.Errorf("Read: stream is not opened yet, ino(%v) offset(%v) size(%v)", inode, offset, size)
		return 0, syscall.EBADF
	}

	s.once.Do(func() {
		s.GetExtents(ctx)
	})

	err = s.IssueFlushRequest()
	if err != nil {
		return
	}

	read, err = s.read(ctx, data, offset, size)
	// log.Errorf("======> ExtentClient Read Exit, inode(%v), time[%v us].", inode, time.Since(t1).Microseconds())
	return
}

func (client *ExtentClient) ReadExtent(ctx context.Context, inode uint64, ek *proto.ExtentKey, data []byte, offset int, size int) (read int, err error, isStream bool) {
	span := proto.SpanFromContext(ctx)
	ctx = proto.ContextWithOperation(ctx, "ReadExtent")
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("read-extent", err, bgTime, 1)
	}()

	var reader *ExtentReader
	var req *ExtentRequest
	if size == 0 {
		return
	}

	s := client.GetStreamer(ctx, inode)
	if s == nil {
		err = fmt.Errorf("Read: stream is not opened yet, ino(%v) ek(%v)", inode, ek)
		return
	}
	err = s.IssueFlushRequest()
	if err != nil {
		return
	}
	reader, err = s.GetExtentReader(ctx, ek)
	if err != nil {
		return
	}

	needCache := false
	cacheKey := util.GenerateKey(s.client.volumeName, s.inode, ek.FileOffset)
	if _, ok := client.inflightL1cache.Load(cacheKey); !ok && client.shouldBcache() {
		client.inflightL1cache.Store(cacheKey, true)
		needCache = true
	}
	defer client.inflightL1cache.Delete(cacheKey)

	// do cache.
	if needCache {
		// read full extent
		buf := make([]byte, ek.Size)
		req = NewExtentRequest(int(ek.FileOffset), int(ek.Size), buf, ek)
		read, err = reader.Read(ctx, req)
		if err != nil {
			return
		}
		read = copy(data, req.Data[offset:offset+size])
		if client.cacheBcache != nil {
			buf := make([]byte, len(req.Data))
			copy(buf, req.Data)
			go func() {
				span.Debugf("ReadExtent L2->L1 Enter cacheKey(%v),client.shouldBcache(%v),needCache(%v)", cacheKey, client.shouldBcache(), needCache)
				if err := client.cacheBcache(ctx, cacheKey, buf); err != nil {
					client.BcacheHealth = false
					span.Debugf("ReadExtent L2->L1 failed, err(%v), set BcacheHealth to false.", err)
				}
				span.Debugf("ReadExtent L2->L1 Exit cacheKey(%v),client.BcacheHealth(%v),needCache(%v)", cacheKey, client.BcacheHealth, needCache)
			}()
		}
		return
	} else {
		// read data by offset:size
		req = NewExtentRequest(int(ek.FileOffset)+offset, size, data, ek)
		ctx := context.Background()
		s.client.readLimiter.Wait(ctx)
		s.client.LimitManager.ReadAlloc(ctx, size)
		isStream = true

		read, err = reader.Read(ctx, req)
		if err != nil {
			return
		}
		read = copy(data, req.Data)
		return
	}
}

// GetStreamer returns the streamer.
func (client *ExtentClient) GetStreamer(ctx context.Context, inode uint64) *Streamer {
	ctx = proto.ContextWithOperation(ctx, "GetStreamer")
	client.streamerLock.Lock()
	defer client.streamerLock.Unlock()
	s, ok := client.streamers[inode]
	if !ok {
		return nil
	}
	if !s.isOpen {
		s.isOpen = true
		s.request = make(chan interface{}, 64)
		s.pendingCache = make(chan bcacheKey, 1)
		go s.server(ctx)
		go s.asyncBlockCache()
	}
	return s
}

func (client *ExtentClient) GetRate() string {
	return fmt.Sprintf("read: %v\nwrite: %v\n", getRate(client.readLimiter), getRate(client.writeLimiter))
}

func (client *ExtentClient) shouldBcache() bool {
	return client.bcacheEnable && client.BcacheHealth
}

func getRate(lim *rate.Limiter) string {
	val := int(lim.Limit())
	if val > 0 {
		return fmt.Sprintf("%v", val)
	}
	return "unlimited"
}

func (client *ExtentClient) SetReadRate(val int) string {
	return setRate(client.readLimiter, val)
}

func (client *ExtentClient) SetWriteRate(val int) string {
	return setRate(client.writeLimiter, val)
}

func setRate(lim *rate.Limiter, val int) string {
	if val > 0 {
		lim.SetLimit(rate.Limit(val))
		return fmt.Sprintf("%v", val)
	}
	lim.SetLimit(rate.Inf)
	return "unlimited"
}

func (client *ExtentClient) Close() error {
	// release streamers
	var inodes []uint64
	client.streamerLock.Lock()
	inodes = make([]uint64, 0, len(client.streamers))
	for inode := range client.streamers {
		inodes = append(inodes, inode)
	}
	client.streamerLock.Unlock()
	for _, inode := range inodes {
		_ = client.EvictStream(inode)
	}
	client.dataWrapper.Stop()
	return nil
}

func (client *ExtentClient) AllocatePreLoadDataPartition(ctx context.Context, volName string, count int, capacity, ttl uint64, zones string) (err error) {
	return client.dataWrapper.AllocatePreLoadDataPartition(ctx, volName, count, capacity, ttl, zones)
}

func (client *ExtentClient) CheckDataPartitionExsit(ctx context.Context, partitionID uint64) error {
	_, err := client.dataWrapper.GetDataPartition(ctx, partitionID)
	return err
}

func (client *ExtentClient) GetDataPartitionForWrite(ctx context.Context) error {
	exclude := make(map[string]struct{})
	_, err := client.dataWrapper.GetDataPartitionForWrite(ctx, exclude)
	return err
}

func (client *ExtentClient) UpdateDataPartitionForColdVolume(ctx context.Context) error {
	return client.dataWrapper.UpdateDataPartition(ctx)
}

func (client *ExtentClient) IsPreloadMode() bool {
	return client.preload
}

func (client *ExtentClient) UploadFlowInfo(ctx context.Context, clientInfo wrapper.SimpleClientInfo) error {
	return client.dataWrapper.UploadFlowInfo(ctx, clientInfo, false)
}
