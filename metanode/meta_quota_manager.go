// Copyright 2023 The CubeFS Authors.
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

package metanode

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/proto"
)

type MetaQuotaManager struct {
	statisticTemp        *sync.Map // key quotaId, value proto.QuotaUsedInfo
	statisticBase        *sync.Map // key quotaId, value proto.QuotaUsedInfo
	statisticRebuildTemp *sync.Map // key quotaId, value proto.QuotaUsedInfo
	statisticRebuildBase *sync.Map // key quotaId, value proto.QuotaUsedInfo
	limitedMap           *sync.Map
	rbuilding            bool
	volName              string
	rwlock               sync.RWMutex
	mpID                 uint64
	enable               bool
}

type MetaQuotaInode struct {
	inode    *Inode
	quotaIds []uint32
}

type TxMetaQuotaInode struct {
	txinode  *TxInode
	quotaIds []uint32
}

func NewQuotaManager(volName string, mpId uint64) (mqMgr *MetaQuotaManager) {
	mqMgr = &MetaQuotaManager{
		statisticTemp:        new(sync.Map),
		statisticBase:        new(sync.Map),
		statisticRebuildTemp: new(sync.Map),
		statisticRebuildBase: new(sync.Map),
		limitedMap:           new(sync.Map),
		volName:              volName,
		mpID:                 mpId,
	}
	return
}

func (qInode *MetaQuotaInode) Marshal(ctx context.Context) (result []byte, err error) {
	var inodeBytes []byte
	quotaBytes := bytes.NewBuffer(make([]byte, 0, 128))
	buff := bytes.NewBuffer(make([]byte, 0, 128))
	inodeBytes, err = qInode.inode.Marshal()
	if err != nil {
		return
	}
	inodeLen := uint32(len(inodeBytes))
	if err = binary.Write(buff, binary.BigEndian, inodeLen); err != nil {
		return
	}
	buff.Write(inodeBytes)
	for _, quotaId := range qInode.quotaIds {
		if err = binary.Write(quotaBytes, binary.BigEndian, quotaId); err != nil {
			return
		}
	}
	buff.Write(quotaBytes.Bytes())
	result = buff.Bytes()
	getSpan(ctx).Debugf("MetaQuotaInode Marshal inode[%v] inodeLen [%v] size [%v]", qInode.inode.Inode, inodeLen, len(result))
	return
}

func (qInode *MetaQuotaInode) Unmarshal(ctx context.Context, raw []byte) (err error) {
	var inodeLen uint32
	var quotaId uint32
	buff := bytes.NewBuffer(raw)
	if err = binary.Read(buff, binary.BigEndian, &inodeLen); err != nil {
		return
	}
	inodeBytes := make([]byte, inodeLen)
	if _, err = buff.Read(inodeBytes); err != nil {
		return
	}
	getSpan(ctx).Debugf("MetaQuotaInode Unmarshal inodeLen [%v] size [%v]", inodeBytes, len(raw))
	qInode.inode = NewInode(0, 0)
	if err = qInode.inode.Unmarshal(inodeBytes); err != nil {
		return
	}
	for {
		if buff.Len() == 0 {
			break
		}
		if err = binary.Read(buff, binary.BigEndian, &quotaId); err != nil {
			return
		}
		qInode.quotaIds = append(qInode.quotaIds, quotaId)
	}
	return
}

func (qInode *TxMetaQuotaInode) Marshal(ctx context.Context) (result []byte, err error) {
	var inodeBytes []byte
	quotaBytes := bytes.NewBuffer(make([]byte, 0, 128))
	buff := bytes.NewBuffer(make([]byte, 0, 128))
	inodeBytes, err = qInode.txinode.Marshal()
	if err != nil {
		return
	}
	inodeLen := uint32(len(inodeBytes))
	if err = binary.Write(buff, binary.BigEndian, inodeLen); err != nil {
		return
	}
	buff.Write(inodeBytes)
	for _, quotaId := range qInode.quotaIds {
		if err = binary.Write(quotaBytes, binary.BigEndian, quotaId); err != nil {
			return
		}
	}
	buff.Write(quotaBytes.Bytes())
	result = buff.Bytes()
	getSpan(ctx).Debugf("TxMetaQuotaInode Marshal inode[%v] inodeLen [%v] size [%v]", qInode.txinode.Inode.Inode, inodeLen, len(result))
	return
}

func (qInode *TxMetaQuotaInode) Unmarshal(ctx context.Context, raw []byte) (err error) {
	var inodeLen uint32
	var quotaId uint32
	buff := bytes.NewBuffer(raw)
	if err = binary.Read(buff, binary.BigEndian, &inodeLen); err != nil {
		return
	}
	inodeBytes := make([]byte, inodeLen)
	if _, err = buff.Read(inodeBytes); err != nil {
		return
	}
	getSpan(ctx).Debugf("TxMetaQuotaInode Unmarshal inodeLen [%v] size [%v]", inodeBytes, len(raw))
	qInode.txinode = NewTxInode(0, 0, nil)
	if err = qInode.txinode.Unmarshal(inodeBytes); err != nil {
		return
	}
	for {
		if buff.Len() == 0 {
			break
		}
		if err = binary.Read(buff, binary.BigEndian, &quotaId); err != nil {
			return
		}
		qInode.quotaIds = append(qInode.quotaIds, quotaId)
	}
	return
}

func (mqMgr *MetaQuotaManager) setQuotaHbInfo(ctx context.Context, infos []*proto.QuotaHeartBeatInfo) {
	mqMgr.rwlock.Lock()
	defer mqMgr.rwlock.Unlock()

	span := getSpan(ctx)
	for _, info := range infos {
		if mqMgr.volName != info.VolName {
			continue
		}
		mqMgr.enable = info.Enable
		mqMgr.limitedMap.Store(info.QuotaId, info.LimitedInfo)
		span.Debugf("mp[%v] quotaId [%v] limitedInfo [%v]", mqMgr.mpID, info.QuotaId, info.LimitedInfo)
	}
	mqMgr.limitedMap.Range(func(key, value interface{}) bool {
		quotaId := key.(uint32)
		found := false

		for _, info := range infos {
			if mqMgr.volName != info.VolName {
				continue
			}
			if info.QuotaId == quotaId {
				found = true
				break
			}
		}

		if !found {
			mqMgr.limitedMap.Delete(quotaId)
		}
		return true
	})
}

func (mqMgr *MetaQuotaManager) getQuotaReportInfos(ctx context.Context) (infos []*proto.QuotaReportInfo) {
	mqMgr.rwlock.Lock()
	defer mqMgr.rwlock.Unlock()
	var usedInfo proto.QuotaUsedInfo
	span := getSpan(ctx).WithOperation(fmt.Sprintf("getQuotaReportInfos-mp.%d", mqMgr.mpID))
	mqMgr.statisticTemp.Range(func(key, value interface{}) bool {
		usedInfo = value.(proto.QuotaUsedInfo)
		if value, isFind := mqMgr.statisticBase.Load(key.(uint32)); isFind {
			baseInfo := value.(proto.QuotaUsedInfo)
			span.Debugf("statisticTemp key [%v] usedInfo [%v] baseInfo [%v]", key.(uint32), usedInfo, baseInfo)
			usedInfo.Add(&baseInfo)
			if usedInfo.UsedFiles < 0 {
				span.Warnf("statisticTemp key [%v] usedInfo [%v]", key.(uint32), usedInfo)
				usedInfo.UsedFiles = 0
			}
			if usedInfo.UsedBytes < 0 {
				span.Warnf("statisticTemp key [%v] usedInfo [%v]", key.(uint32), usedInfo)
				usedInfo.UsedBytes = 0
			}
		}
		mqMgr.statisticBase.Store(key.(uint32), usedInfo)
		return true
	})
	mqMgr.statisticTemp = new(sync.Map)
	mqMgr.statisticBase.Range(func(key, value interface{}) bool {
		quotaId := key.(uint32)
		if _, ok := mqMgr.limitedMap.Load(quotaId); !ok {
			return true
		}
		usedInfo = value.(proto.QuotaUsedInfo)
		reportInfo := &proto.QuotaReportInfo{
			QuotaId:  quotaId,
			UsedInfo: usedInfo,
		}
		infos = append(infos, reportInfo)
		span.Debugf("statisticBase key [%v] usedInfo [%v]", key.(uint32), usedInfo)
		return true
	})
	return
}

func (mqMgr *MetaQuotaManager) statisticRebuildStart() bool {
	mqMgr.rwlock.Lock()
	defer mqMgr.rwlock.Unlock()
	if !mqMgr.enable {
		return false
	}

	if mqMgr.rbuilding {
		return false
	}
	mqMgr.rbuilding = true
	return true
}

func (mqMgr *MetaQuotaManager) statisticRebuildFin(ctx context.Context, rebuild bool) {
	mqMgr.rwlock.Lock()
	defer mqMgr.rwlock.Unlock()
	mqMgr.rbuilding = false
	if !rebuild {
		mqMgr.statisticRebuildBase = new(sync.Map)
		mqMgr.statisticRebuildTemp = new(sync.Map)
		return
	}
	mqMgr.statisticBase = mqMgr.statisticRebuildBase
	mqMgr.statisticTemp = mqMgr.statisticRebuildTemp
	mqMgr.statisticRebuildBase = new(sync.Map)
	mqMgr.statisticRebuildTemp = new(sync.Map)

	if log.GetOutputLevel() >= log.Linfo {
		span := getSpan(ctx)
		mqMgr.statisticTemp.Range(func(key, value interface{}) bool {
			quotaId := key.(uint32)
			usedInfo := value.(proto.QuotaUsedInfo)
			span.Infof("statisticRebuildFin statisticTemp mp[%v] quotaId [%v] usedInfo [%v]", mqMgr.mpID, quotaId, usedInfo)
			return true
		})
		mqMgr.statisticBase.Range(func(key, value interface{}) bool {
			quotaId := key.(uint32)
			usedInfo := value.(proto.QuotaUsedInfo)
			span.Infof("statisticRebuildFin statisticBase mp[%v] quotaId [%v] usedInfo [%v]", mqMgr.mpID, quotaId, usedInfo)
			return true
		})
	}
}

func (mqMgr *MetaQuotaManager) IsOverQuota(ctx context.Context, size bool, files bool, quotaId uint32) (status uint8) {
	var limitedInfo proto.QuotaLimitedInfo
	mqMgr.rwlock.RLock()
	defer mqMgr.rwlock.RUnlock()
	if !mqMgr.enable {
		getSpan(ctx).Infof("IsOverQuota quota [%v] is disable.", quotaId)
		return
	}
	value, isFind := mqMgr.limitedMap.Load(quotaId)
	if isFind {
		limitedInfo = value.(proto.QuotaLimitedInfo)
		if size && limitedInfo.LimitedBytes {
			status = proto.OpNoSpaceErr
		}

		if files && limitedInfo.LimitedFiles {
			status = proto.OpNoSpaceErr
		}
	}
	getSpan(ctx).Infof("IsOverQuota quotaId [%v] limitedInfo[%v] status [%v] isFind [%v]", quotaId, limitedInfo, status, isFind)
	return
}

func (mqMgr *MetaQuotaManager) updateUsedInfo(ctx context.Context, size int64, files int64, quotaId uint32) {
	var baseInfo proto.QuotaUsedInfo
	var baseTemp proto.QuotaUsedInfo
	mqMgr.rwlock.Lock()
	defer mqMgr.rwlock.Unlock()

	value, isFind := mqMgr.statisticTemp.Load(quotaId)
	if isFind {
		baseInfo = value.(proto.QuotaUsedInfo)
	}
	baseInfo.UsedBytes += size
	baseInfo.UsedFiles += files
	mqMgr.statisticTemp.Store(quotaId, baseInfo)
	if mqMgr.rbuilding {
		value, isFind = mqMgr.statisticRebuildTemp.Load(quotaId)
		if isFind {
			baseTemp = value.(proto.QuotaUsedInfo)
		} else {
			baseTemp.UsedBytes = 0
			baseTemp.UsedFiles = 0
		}
		baseTemp.UsedBytes += size
		baseTemp.UsedFiles += files
		mqMgr.statisticRebuildTemp.Store(quotaId, baseTemp)
	}
	getSpan(ctx).Debugf("updateUsedInfo mpId [%v] quotaId [%v] baseInfo [%v] baseTemp[%v]", mqMgr.mpID, quotaId, baseInfo, baseTemp)
}

func (mqMgr *MetaQuotaManager) EnableQuota() bool {
	return mqMgr.enable
}

func (mqMgr *MetaQuotaManager) getUsedInfoForTest(quotaId uint32) (size int64, files int64) {
	mqMgr.rwlock.Lock()
	defer mqMgr.rwlock.Unlock()
	var baseInfo proto.QuotaUsedInfo
	value, isFind := mqMgr.statisticTemp.Load(quotaId)
	if isFind {
		baseInfo = value.(proto.QuotaUsedInfo)
	}
	return baseInfo.UsedBytes, baseInfo.UsedFiles
}
