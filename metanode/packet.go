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
	"encoding/json"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/storage"
	"github.com/cubefs/cubefs/util/log"
)

type Packet struct {
	proto.Packet
}

// NewPacketToDeleteExtent returns a new packet to delete the extent.
func NewPacketToDeleteExtent(dp *DataPartition, ext *proto.ExtentKey) *Packet {
	p := new(Packet)
	p.Magic = proto.ProtoMagic
	p.Opcode = proto.OpMarkDelete
	p.ExtentType = proto.NormalExtentType
	p.PartitionID = uint64(dp.PartitionID)
	if storage.IsTinyExtent(ext.ExtentId) {
		p.ExtentType = proto.TinyExtentType
	}

	p.Data, _ = json.Marshal(ext)
	p.Size = uint32(len(p.Data))

	if ext.IsSplit() {
		log.LogDebugf("ext [%v] delete be set split flag", ext)
		p.Opcode = proto.OpSplitMarkDelete
	} else {
		log.LogDebugf("ext [%v] delete normal ext", ext)
	}
	p.ExtentID = ext.ExtentId
	p.ReqID = proto.GenerateRequestID()
	p.RemainingFollowers = uint8(len(dp.Hosts) - 1)
	if len(dp.Hosts) == 1 {
		p.RemainingFollowers = 127
	}

	p.Arg = ([]byte)(dp.GetAllAddrs())
	p.ArgLen = uint32(len(p.Arg))

	return p
}

// NewPacketToBatchDeleteExtent returns a new packet to batch delete the extent.
func NewPacketToBatchDeleteExtent(dp *DataPartition, exts []*proto.ExtentKey) *Packet {
	p := new(Packet)
	p.Magic = proto.ProtoMagic
	p.Opcode = proto.OpBatchDeleteExtent
	p.ExtentType = proto.NormalExtentType
	p.PartitionID = uint64(dp.PartitionID)
	p.Data, _ = json.Marshal(exts)
	p.Size = uint32(len(p.Data))
	p.ReqID = proto.GenerateRequestID()
	p.RemainingFollowers = uint8(len(dp.Hosts) - 1)
	if len(dp.Hosts) == 1 {
		p.RemainingFollowers = 127
	}
	p.Arg = ([]byte)(dp.GetAllAddrs())
	p.ArgLen = uint32(len(p.Arg))

	return p
}

// NewPacketToDeleteExtent returns a new packet to delete the extent.
func NewPacketToFreeInodeOnRaftFollower(partitionID uint64, freeInodes []byte) *Packet {
	p := new(Packet)
	p.Magic = proto.ProtoMagic
	p.Opcode = proto.OpMetaFreeInodesOnRaftFollower
	p.PartitionID = partitionID
	p.ExtentType = proto.NormalExtentType
	p.ReqID = proto.GenerateRequestID()
	p.Data = make([]byte, len(freeInodes))
	copy(p.Data, freeInodes)
	p.Size = uint32(len(p.Data))

	return p
}
