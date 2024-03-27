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

package metanode

import (
	"context"
	"strings"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/btree"
)

type DentryResponse struct {
	Status uint8
	Msg    *Dentry
}

func NewDentryResponse() *DentryResponse {
	return &DentryResponse{
		Msg: &Dentry{},
	}
}

func (mp *metaPartition) fsmTxCreateDentry(ctx context.Context, txDentry *TxDentry) (status uint8) {
	done := mp.txProcessor.txManager.txInRMDone(txDentry.TxInfo.TxID)
	if done {
		getSpan(ctx).Warnf("tx is already finish. txId %s", txDentry.TxInfo.TxID)
		status = proto.OpTxInfoNotExistErr
		return
	}

	txDI := proto.NewTxDentryInfo("", txDentry.Dentry.ParentId, txDentry.Dentry.Name, 0)
	txDenInfo, ok := txDentry.TxInfo.TxDentryInfos[txDI.GetKey()]
	if !ok {
		status = proto.OpTxDentryInfoNotExistErr
		return
	}

	rbDentry := NewTxRollbackDentry(txDentry.Dentry, txDenInfo, TxDelete)
	status = mp.txProcessor.txResource.addTxRollbackDentry(ctx, rbDentry)
	if status == proto.OpExistErr {
		return proto.OpOk
	}

	if status != proto.OpOk {
		return
	}

	defer func() {
		if status != proto.OpOk {
			mp.txProcessor.txResource.deleteTxRollbackDentry(ctx, txDenInfo.ParentId, txDenInfo.Name, txDenInfo.TxID)
		}
	}()

	return mp.fsmCreateDentry(ctx, txDentry.Dentry, false)
}

// Insert a dentry into the dentry tree.
func (mp *metaPartition) fsmCreateDentry(ctx context.Context, dentry *Dentry, forceUpdate bool) (status uint8) {
	span := spanOperationf(getSpan(ctx), "fsmCreateDentry-mp(%d)", mp.config.PartitionId)
	status = proto.OpOk
	var parIno *Inode
	if !forceUpdate {
		item := mp.inodeTree.CopyGet(NewInode(dentry.ParentId, 0))
		if item == nil {
			span.Errorf("ParentId [%v] get nil, dentry name [%v], inode[%v]", dentry.ParentId, dentry.Name, dentry.Inode)
			status = proto.OpNotExistErr
			return
		}
		parIno = item.(*Inode)
		if parIno.ShouldDelete() {
			span.Errorf("ParentId [%v] get [%v] but should del, dentry name [%v], inode[%v]",
				dentry.ParentId, parIno, dentry.Name, dentry.Inode)
			status = proto.OpNotExistErr
			return
		}
		if !proto.IsDir(parIno.Type) {
			span.Errorf("ParentId [%v] get [%v] but should del, dentry name [%v], inode[%v]",
				dentry.ParentId, parIno, dentry.Name, dentry.Inode)
			status = proto.OpArgMismatchErr
			return
		}
	}

	if item, ok := mp.dentryTree.ReplaceOrInsert(dentry, false); !ok {
		// do not allow directories and files to overwrite each
		// other when renaming
		d := item.(*Dentry)
		if d.isDeleted() {
			span.Debugf("newest dentry %v be set deleted flag", d)
			d.Inode = dentry.Inode
			if d.getVerSeq() == dentry.getVerSeq() {
				d.setVerSeq(dentry.getSeqFiled())
			} else {
				if d.getSnapListLen() > 0 && d.multiSnap.dentryList[0].isDeleted() {
					d.setVerSeq(dentry.getSeqFiled())
				} else {
					d.addVersion(dentry.getSeqFiled())
				}
			}
			d.Type = dentry.Type
			d.ParentId = dentry.ParentId
			span.Debugf("latest dentry already deleted.Now create new one [%v]", dentry)

			if !forceUpdate {
				parIno.IncNLink(ctx, mp.verSeq)
				parIno.SetMtime()
			}
			return
		} else if proto.OsModeType(dentry.Type) != proto.OsModeType(d.Type) && !proto.IsSymlink(dentry.Type) && !proto.IsSymlink(d.Type) {
			span.Errorf("ParentId [%v] get [%v] but should del, dentry name [%v], inode[%v], type[%v,%v],dir[%v,%v]",
				dentry.ParentId, parIno, dentry.Name, dentry.Inode, dentry.Type, d.Type, proto.IsSymlink(dentry.Type), proto.IsSymlink(d.Type))
			status = proto.OpArgMismatchErr
			return
		} else if dentry.ParentId == d.ParentId && strings.Compare(dentry.Name, d.Name) == 0 && dentry.Inode == d.Inode {
			span.Debugf("ver no need repeat create new one [%v]", dentry)
			return
		}
		span.Errorf("ver dentry already exist [%v] and diff with the request [%v]", d, dentry)
		status = proto.OpExistErr
		return
	}

	if !forceUpdate {
		parIno.IncNLink(ctx, mp.verSeq)
		parIno.SetMtime()
	}
	return
}

func (mp *metaPartition) getDentryList(dentry *Dentry) (denList []proto.DetryInfo) {
	item := mp.dentryTree.Get(dentry)
	if item != nil {
		if item.(*Dentry).getSnapListLen() == 0 {
			return
		}
		for _, den := range item.(*Dentry).multiSnap.dentryList {
			denList = append(denList, proto.DetryInfo{
				Inode:  den.Inode,
				Mode:   den.Type,
				IsDel:  den.isDeleted(),
				VerSeq: den.getVerSeq(),
			})
		}
	}
	return
}

// Query a dentry from the dentry tree with specified dentry info.
func (mp *metaPartition) getDentry(ctx context.Context, dentry *Dentry) (*Dentry, uint8) {
	item := mp.dentryTree.Get(dentry)
	if item == nil {
		return nil, proto.OpNotExistErr
	}
	getSpan(ctx).Debugf("action[getDentry] get dentry[%v] by req dentry %v", item.(*Dentry), dentry)
	den := mp.getDentryByVerSeq(ctx, item.(*Dentry), dentry.getSeqFiled())
	if den != nil {
		return den, proto.OpOk
	}
	return den, proto.OpNotExistErr
}

func (mp *metaPartition) fsmTxDeleteDentry(ctx context.Context, txDentry *TxDentry) (resp *DentryResponse) {
	resp = NewDentryResponse()
	resp.Status = proto.OpOk
	if mp.txProcessor.txManager.txInRMDone(txDentry.TxInfo.TxID) {
		getSpan(ctx).Warnf("tx is already finish. txId %s", txDentry.TxInfo.TxID)
		resp.Status = proto.OpTxInfoNotExistErr
		return
	}

	tmpDen := txDentry.Dentry
	txDI := proto.NewTxDentryInfo("", tmpDen.ParentId, tmpDen.Name, 0)
	txDenInfo, ok := txDentry.TxInfo.TxDentryInfos[txDI.GetKey()]
	if !ok {
		resp.Status = proto.OpTxDentryInfoNotExistErr
		return
	}

	rbDentry := NewTxRollbackDentry(tmpDen, txDenInfo, TxAdd)
	resp.Status = mp.txProcessor.txResource.addTxRollbackDentry(ctx, rbDentry)
	if resp.Status == proto.OpExistErr {
		resp.Status = proto.OpOk
		return
	}

	if resp.Status != proto.OpOk {
		return
	}

	defer func() {
		if resp.Status != proto.OpOk {
			mp.txProcessor.txResource.deleteTxRollbackDentry(ctx, txDenInfo.ParentId, txDenInfo.Name, txDenInfo.TxID)
		}
	}()

	item := mp.dentryTree.Get(tmpDen)
	if item == nil || item.(*Dentry).Inode != tmpDen.Inode {
		getSpan(ctx).Warnf("got wrong dentry, want %v, got %v", tmpDen, item)
		resp.Status = proto.OpNotExistErr
		return
	}

	mp.dentryTree.Delete(tmpDen)
	// parent link count not change
	resp.Msg = item.(*Dentry)
	return
}

// Delete dentry from the dentry tree.
func (mp *metaPartition) fsmDeleteDentry(ctx context.Context, denParm *Dentry, checkInode bool) (resp *DentryResponse) {
	span := spanOperationf(getSpan(ctx), "fsmDeleteDentry-mp(%d)", mp.config.PartitionId)
	span.Debugf("delete param (%v) seq [%v]", denParm, denParm.getSeqFiled())
	resp = NewDentryResponse()
	resp.Status = proto.OpOk
	var (
		denFound *Dentry
		item     interface{}
		doMore   = true
		clean    bool
	)
	if checkInode {
		span.Debugf("delete param %v", denParm)
		item = mp.dentryTree.Execute(func(tree *btree.BTree) interface{} {
			d := tree.CopyGet(denParm)
			if d == nil {
				return nil
			}
			den := d.(*Dentry)
			if den.Inode != denParm.Inode {
				return nil
			}
			if mp.verSeq == 0 {
				span.Debug("volume snapshot not enabled, delete directly")
				denFound = den
				return mp.dentryTree.tree.Delete(den)
			}
			denFound, doMore, clean = den.deleteVerSnapshot(ctx, denParm.getSeqFiled(), mp.verSeq, mp.GetVerList())
			return den
		})
	} else {
		span.Debugf("denParm dentry %v", denParm)
		if mp.verSeq == 0 {
			item = mp.dentryTree.Delete(denParm)
			if item != nil {
				denFound = item.(*Dentry)
			}
		} else {
			item = mp.dentryTree.Get(denParm)
			if item != nil {
				denFound, doMore, clean = item.(*Dentry).deleteVerSnapshot(ctx, denParm.getSeqFiled(), mp.verSeq, mp.GetVerList())
			}
		}
	}

	if item != nil && (clean || (item.(*Dentry).getSnapListLen() == 0 && item.(*Dentry).isDeleted())) {
		span.Debugf("dnetry %v really be deleted", item.(*Dentry))
		item = mp.dentryTree.Delete(item.(*Dentry))
		span.Debugf("dnetry %v been deleted", item.(*Dentry))
	}

	if !doMore { // not the top layer,do nothing to parent inode
		if denFound != nil {
			resp.Msg = denFound
		}
		span.Debugf("there's nothing to do more denParm %v", denParm)
		return
	}
	if denFound == nil {
		resp.Status = proto.OpNotExistErr
		span.Errorf("not found dentry %v", denParm)
		return
	} else {
		mp.inodeTree.CopyFind(NewInode(denParm.ParentId, 0),
			func(item BtreeItem) {
				if item != nil { // no matter
					ino := item.(*Inode)
					if !ino.ShouldDelete() {
						span.Debugf("den %v delete parent's link", denParm)
						if denParm.getSeqFiled() == 0 {
							item.(*Inode).DecNLink()
						}
						span.Debugf("inode[%v] be unlinked by child name %v", item.(*Inode).Inode, denParm.Name)
						item.(*Inode).SetMtime()
					}
				}
			})
	}
	resp.Msg = denFound
	return
}

// batch Delete dentry from the dentry tree.
func (mp *metaPartition) fsmBatchDeleteDentry(ctx context.Context, db DentryBatch) []*DentryResponse {
	result := make([]*DentryResponse, 0, len(db))
	for _, dentry := range db {
		status := mp.dentryInTx(ctx, dentry.ParentId, dentry.Name)
		if status != proto.OpOk {
			result = append(result, &DentryResponse{Status: status})
			continue
		}
		result = append(result, mp.fsmDeleteDentry(ctx, dentry, true))
	}
	return result
}

func (mp *metaPartition) fsmTxUpdateDentry(ctx context.Context, txUpDateDentry *TxUpdateDentry) (resp *DentryResponse) {
	resp = NewDentryResponse()
	resp.Status = proto.OpOk

	if mp.txProcessor.txManager.txInRMDone(txUpDateDentry.TxInfo.TxID) {
		getSpan(ctx).Warnf("tx is already finish. txId %s", txUpDateDentry.TxInfo.TxID)
		resp.Status = proto.OpTxInfoNotExistErr
		return
	}

	newDen := txUpDateDentry.NewDentry
	oldDen := txUpDateDentry.OldDentry

	txDI := proto.NewTxDentryInfo("", oldDen.ParentId, oldDen.Name, 0)
	txDenInfo, ok := txUpDateDentry.TxInfo.TxDentryInfos[txDI.GetKey()]
	if !ok {
		resp.Status = proto.OpTxDentryInfoNotExistErr
		return
	}

	item := mp.dentryTree.CopyGet(oldDen)
	if item == nil || item.(*Dentry).Inode != oldDen.Inode {
		resp.Status = proto.OpNotExistErr
		getSpan(ctx).Warnf("find dentry is not right, want %v, got %v", oldDen, item)
		return
	}

	rbDentry := NewTxRollbackDentry(txUpDateDentry.OldDentry, txDenInfo, TxUpdate)
	resp.Status = mp.txProcessor.txResource.addTxRollbackDentry(ctx, rbDentry)
	if resp.Status == proto.OpExistErr {
		resp.Status = proto.OpOk
		return
	}

	if resp.Status != proto.OpOk {
		return
	}

	d := item.(*Dentry)
	d.Inode, newDen.Inode = newDen.Inode, d.Inode
	resp.Msg = newDen
	return
}

func (mp *metaPartition) fsmUpdateDentry(ctx context.Context, dentry *Dentry) (resp *DentryResponse) {
	resp = NewDentryResponse()
	resp.Status = proto.OpOk
	mp.dentryTree.CopyFind(dentry, func(item BtreeItem) {
		if item == nil {
			resp.Status = proto.OpNotExistErr
			return
		}
		d := item.(*Dentry)
		if dentry.Inode == d.Inode {
			return
		}
		if d.getVerSeq() < mp.GetVerSeq() {
			dn := d.CopyDirectly()
			dn.(*Dentry).setVerSeq(d.getVerSeq())
			d.setVerSeq(mp.GetVerSeq())
			d.multiSnap.dentryList = append([]*Dentry{dn.(*Dentry)}, d.multiSnap.dentryList...)
		}
		d.Inode, dentry.Inode = dentry.Inode, d.Inode
		resp.Msg = dentry
	})
	return
}

func (mp *metaPartition) getDentryByVerSeq(ctx context.Context, dy *Dentry, verSeq uint64) (d *Dentry) {
	d, _ = dy.getDentryFromVerList(ctx, verSeq, false)
	return
}

func (mp *metaPartition) readDirOnly(ctx context.Context, req *ReadDirOnlyReq) (resp *ReadDirOnlyResp) {
	resp = &ReadDirOnlyResp{}
	begDentry := &Dentry{
		ParentId: req.ParentID,
	}
	endDentry := &Dentry{
		ParentId: req.ParentID + 1,
	}
	mp.dentryTree.AscendRange(begDentry, endDentry, func(i BtreeItem) bool {
		if proto.IsDir(i.(*Dentry).Type) {
			d := mp.getDentryByVerSeq(ctx, i.(*Dentry), req.VerSeq)
			if d == nil {
				return true
			}
			resp.Children = append(resp.Children, proto.Dentry{
				Inode: d.Inode,
				Type:  d.Type,
				Name:  d.Name,
			})
		}
		return true
	})
	return
}

func (mp *metaPartition) readDir(ctx context.Context, req *ReadDirReq) (resp *ReadDirResp) {
	resp = &ReadDirResp{}
	begDentry := &Dentry{
		ParentId: req.ParentID,
	}
	endDentry := &Dentry{
		ParentId: req.ParentID + 1,
	}
	mp.dentryTree.AscendRange(begDentry, endDentry, func(i BtreeItem) bool {
		d := mp.getDentryByVerSeq(ctx, i.(*Dentry), req.VerSeq)
		if d == nil {
			return true
		}
		resp.Children = append(resp.Children, proto.Dentry{
			Inode: d.Inode,
			Type:  d.Type,
			Name:  d.Name,
		})
		return true
	})
	return
}

// Read dentry from btree by limit count
// if req.Marker == "" and req.Limit == 0, it becomes readDir
// else if req.Marker != "" and req.Limit == 0, return dentries from pid:name to pid+1
// else if req.Marker == "" and req.Limit != 0, return dentries from pid with limit count
// else if req.Marker != "" and req.Limit != 0, return dentries from pid:marker to pid:xxxx with limit count
func (mp *metaPartition) readDirLimit(ctx context.Context, req *ReadDirLimitReq) (resp *ReadDirLimitResp) {
	span := getSpan(ctx)
	span.Debugf("action[readDirLimit] mp[%v] req %v", mp.config.PartitionId, req)
	resp = &ReadDirLimitResp{}
	startDentry := &Dentry{
		ParentId: req.ParentID,
	}
	if len(req.Marker) > 0 {
		startDentry.Name = req.Marker
	}
	endDentry := &Dentry{
		ParentId: req.ParentID + 1,
	}
	mp.dentryTree.AscendRange(startDentry, endDentry, func(i BtreeItem) bool {
		if !proto.IsDir(i.(*Dentry).Type) && (req.VerOpt&uint8(proto.FlagsSnapshotDel) > 0) {
			if req.VerOpt&uint8(proto.FlagsSnapshotDelDir) > 0 {
				return true
			}
			if !i.(*Dentry).isEffective(req.VerSeq) {
				return true
			}
		}
		d := mp.getDentryByVerSeq(ctx, i.(*Dentry), req.VerSeq)
		if d == nil {
			return true
		}
		resp.Children = append(resp.Children, proto.Dentry{
			Inode: d.Inode,
			Type:  d.Type,
			Name:  d.Name,
		})
		// Limit == 0 means no limit.
		if req.Limit > 0 && uint64(len(resp.Children)) >= req.Limit {
			return false
		}
		return true
	})
	span.Debugf("action[readDirLimit] mp[%v] resp %v", mp.config.PartitionId, resp)
	return
}
