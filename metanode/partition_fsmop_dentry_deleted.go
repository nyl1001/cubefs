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

package metanode

import (
	"github.com/chubaofs/chubaofs/proto"
	"github.com/chubaofs/chubaofs/util/log"
	"strings"
)

type fsmOpDeletedDentryResponse struct {
	Status uint8          `json:"st"`
	Msg    *DeletedDentry `json:"den"`
}

func (mp *metaPartition) mvToDeletedDentryTree(dentry *Dentry, timestamp int64, from string) (uint8, error) {
	ddentry := newDeletedDentry(dentry, timestamp, from)
	resp, err := mp.fsmCreateDeletedDentry(ddentry, false)
	if err != nil {
		log.LogErrorf("action[mvToDeletedDentryTree] create deleted dentry failed:%v", err)
	}
	return resp.Status, err
}

func (mp *metaPartition) fsmCreateDeletedDentry(ddentry *DeletedDentry, force bool) (rsp *fsmOpDeletedDentryResponse, err error) {
	rsp = new(fsmOpDeletedDentryResponse)
	rsp.Status = proto.OpOk
	err = mp.dentryDeletedTree.Create(ddentry, force)
	if err == nil {
		return
	}
	if err != existsError {
		rsp.Status = proto.OpErr
		return
	}
	//get and validate
	//todo:add check logical
	var dden *DeletedDentry
	if dden, err = mp.dentryDeletedTree.Get(ddentry.ParentId, ddentry.Name, ddentry.Timestamp); err != nil {
		rsp.Status = proto.OpErr
		return
	}
	if dden == nil {
		//todo:record log, alarm?
		log.LogErrorf("action[fsmCreateDeletedDentry] deleted dentry(%v) get nil", ddentry)
		return
	}

	if dden.ParentId == ddentry.ParentId && strings.Compare(dden.Name, ddentry.Name) == 0 &&
		dden.Timestamp == ddentry.Timestamp && dden.Inode == ddentry.Inode {
		return
	}
	rsp.Status = proto.OpErr
	return
}

func (mp *metaPartition) fsmBatchCleanDeletedDentry(dens DeletedDentryBatch) (
	res []*fsmOpDeletedDentryResponse, err error) {

	res = make([]*fsmOpDeletedDentryResponse, 0)
	wrongIndex := len(dens)
	defer func() {
		for index := wrongIndex; index < len(dens); index++ {
			res = append(res, &fsmOpDeletedDentryResponse{Status: proto.OpErr})
		}
	}()

	for index := 0; index < len(dens); index++ {
		var rsp *fsmOpDeletedDentryResponse
		rsp, err = mp.cleanDeletedDentry(dens[index])
		if err == rocksdbError {
			wrongIndex = index
			break
		}
		if rsp.Status != proto.OpOk {
			res = append(res, rsp)
		}
	}
	return
}

func (mp *metaPartition) fsmCleanDeletedDentry(dd *DeletedDentry) (resp *fsmOpDeletedDentryResponse, err error) {
	return mp.cleanDeletedDentry(dd)
}

func (mp *metaPartition) cleanDeletedDentry(ddentry *DeletedDentry) (
	resp *fsmOpDeletedDentryResponse, err error) {
	resp = new(fsmOpDeletedDentryResponse)
	resp.Msg = ddentry
	resp.Status = proto.OpOk

	defer func() {
		log.LogDebugf("[cleanDeletedDentry], dentry: %v, status: %v", ddentry, resp.Status)
	}()

	if len(ddentry.Name) == 0 {
		log.LogErrorf("[cleanDeletedDentry], not found name: %v", ddentry)
		resp.Status = proto.OpErr
		return
	}

	var dd *DeletedDentry
	dd, err = mp.dentryDeletedTree.Get(ddentry.ParentId, ddentry.Name, ddentry.Timestamp)
	if err != nil {
		log.LogErrorf("[cleanDeletedDentry], not found dentry: %v", ddentry)
		resp.Status = proto.OpErr
		return
	}
	if dd == nil {
		log.LogErrorf("[cleanDeletedDentry], not found dentry: %v", ddentry)
		resp.Status = proto.OpNotExistErr
		return
	}

	if dd.Inode != ddentry.Inode {
		log.LogErrorf("[cleanDeletedDentry], not found dentry: %v, item: %v", ddentry, dd)
		resp.Status = proto.OpNotExistErr
	}
	if _, err = mp.dentryDeletedTree.Delete(dd.ParentId, dd.Name, dd.Timestamp); err != nil && err != notExistsError {
		resp.Status = proto.OpErr
		return
	}
	resp.Msg = dd
	return
}

func (mp *metaPartition) fsmRecoverDeletedDentry(ddentry *DeletedDentry) (
	resp *fsmOpDeletedDentryResponse, err error) {
	resp = new(fsmOpDeletedDentryResponse)
	resp.Msg = ddentry
	resp.Status = proto.OpOk

	defer func() {
		log.LogDebugf("[fsmRecoverDeletedDentry], dentry: %v, status: %v", ddentry, resp.Status)
	}()

	if len(ddentry.Name) == 0 {
		log.LogErrorf("[fsmRecoverDeletedDentry], not found name in  dentry: %v", ddentry)
		resp.Status = proto.OpErr
		return
	}
	var dd *DeletedDentry
	var dentry *Dentry
	den := ddentry.buildDentry()
	dd, err = mp.dentryDeletedTree.RefGet(ddentry.ParentId, ddentry.Name, ddentry.Timestamp)
	if err != nil {
		log.LogErrorf("[fsmRecoverDeletedDentry], get dentry(%v) failed, err(%v)", ddentry, err)
		resp.Status = proto.OpErr
		return
	}
	if dd == nil {
		dentry, err = mp.dentryTree.RefGet(den.ParentId, den.Name)
		if dentry != nil {
			return
		}

		den.Name = appendTimestampToName(ddentry.Name, ddentry.Timestamp)
		dentry, err = mp.dentryTree.RefGet(den.ParentId, den.Name)
		if dentry != nil {
			return
		}
		resp.Status = proto.OpNotExistErr
		log.LogErrorf("[fsmRecoverDeletedDentry], not found dentry: %v", ddentry)
		return
	}
	if dd.Inode != ddentry.Inode {
		log.LogErrorf("[fsmRecoverDeletedDentry], den: %v, %v", ddentry, dd)
		resp.Status = proto.OpNotExistErr
		return
	}

	dentry, err = mp.dentryTree.RefGet(den.ParentId, den.Name)
	if err != nil {
		log.LogErrorf("[fsmRecoverDeletedDentry] get detry: %v failed:%v", den, err)
		resp.Status = proto.OpErr
		return
	}
	if dentry != nil {
		newDDentry := dd.Copy().(*DeletedDentry)
		newDDentry.appendTimestampToName()
		d := newDDentry.buildDentry()
		dentry, err = mp.dentryTree.Get(d.ParentId, d.Name)
		if err != nil {
			log.LogErrorf("[fsmRecoverDeletedDentry], get dentry: %v failed:%v", newDDentry, err)
			resp.Status = proto.OpErr
			return
		}
		if dentry != nil {
			log.LogErrorf("[fsmRecoverDeletedDentry], the dentry: %v has been exist", newDDentry)
			resp.Status = proto.OpExistErr
			return
		}

		resp.Status, err = mp.fsmCreateDentry(d, false)
		resp.Msg = newDDentry
		if err != nil || resp.Status != proto.OpOk {
			log.LogErrorf("[fsmRecoverDeletedDentry], failed to create dentry: %v, status: %v, err :%v",
				newDDentry, resp.Status, err)
			return
		}
	} else {
		dentry = dd.buildDentry()
		resp.Status, err = mp.fsmCreateDentry(dentry, false)
		if err != nil || resp.Status != proto.OpOk {
			log.LogErrorf("[fsmRecoverDeletedDentry], failed to create dentry: %v, status: %v, err: %v", dentry, resp.Status, err)
			return
		}
		resp.Msg = dd
	}
	if _, err = mp.dentryDeletedTree.Delete(ddentry.ParentId, ddentry.Name, ddentry.Timestamp); err != nil {
		log.LogErrorf("[fsmRecoverDeletedDentry] deleted dentry: %v delete failed:%v", ddentry, err)
		resp.Status = proto.OpErr
		return
	}
	return
}

func (mp *metaPartition) fsmBatchRecoverDeletedDentry(dens DeletedDentryBatch) (
	res []*fsmOpDeletedDentryResponse, err error) {
	res = make([]*fsmOpDeletedDentryResponse, 0)
	var wrongIndex = len(dens)
	defer func() {
		for index := wrongIndex; index < len(dens); index++ {
			res = append(res, &fsmOpDeletedDentryResponse{Status: proto.OpErr, Msg: dens[index]})
		}
	}()
	for index, den := range dens {
		var rsp *fsmOpDeletedDentryResponse
		rsp, err = mp.fsmRecoverDeletedDentry(den)
		if err == rocksdbError {
			wrongIndex = index
		}
		if rsp.Status != proto.OpOk {
			res = append(res, rsp)
		}
	}
	return
}

func (mp *metaPartition) fsmCleanExpiredDentry(dens DeletedDentryBatch) (
	res []*fsmOpDeletedDentryResponse, err error) {
	res = make([]*fsmOpDeletedDentryResponse, 0)
	var wrongIndex = len(dens)
	defer func() {
		for index := wrongIndex; index < len(dens); index++ {
			res = append(res, &fsmOpDeletedDentryResponse{Status: proto.OpErr, Msg: dens[index]})
		}
	}()
	for index, den := range dens {
		var rsp *fsmOpDeletedDentryResponse
		rsp, err = mp.cleanDeletedDentry(den)
		if err == rocksdbError {
			wrongIndex = index
			break
		}
		if rsp.Status != proto.OpOk {
			res = append(res, rsp)
		}
	}
	return
}
