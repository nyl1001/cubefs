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

package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cubefs/cubefs/util/log"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/buf"
)

var (
	GRequestID = int64(1)
	Buffers    *buf.BufferPool
)

// GenerateRequestID generates the request ID.
func GenerateRequestID() int64 {
	return atomic.AddInt64(&GRequestID, 1)
}

const (
	AddrSplit = "/"
)

// Operations
const (
	ProtoMagic           uint8 = 0xFF
	OpInitResultCode     uint8 = 0x00
	OpCreateExtent       uint8 = 0x01
	OpMarkDelete         uint8 = 0x02
	OpWrite              uint8 = 0x03
	OpRead               uint8 = 0x04
	OpStreamRead         uint8 = 0x05
	OpStreamFollowerRead uint8 = 0x06
	OpGetAllWatermarks   uint8 = 0x07

	OpNotifyReplicasToRepair         uint8 = 0x08
	OpExtentRepairRead               uint8 = 0x09
	OpBroadcastMinAppliedID          uint8 = 0x0A
	OpRandomWrite                    uint8 = 0x0F
	OpGetAppliedId                   uint8 = 0x10
	OpGetPartitionSize               uint8 = 0x11
	OpSyncRandomWrite                uint8 = 0x12
	OpSyncWrite                      uint8 = 0x13
	OpReadTinyDeleteRecord           uint8 = 0x14
	OpTinyExtentRepairRead           uint8 = 0x15
	OpGetMaxExtentIDAndPartitionSize uint8 = 0x16

	// Operations: Client -> MetaNode.
	OpMetaCreateInode   uint8 = 0x20
	OpMetaUnlinkInode   uint8 = 0x21
	OpMetaCreateDentry  uint8 = 0x22
	OpMetaDeleteDentry  uint8 = 0x23
	OpMetaOpen          uint8 = 0x24
	OpMetaLookup        uint8 = 0x25
	OpMetaReadDir       uint8 = 0x26
	OpMetaInodeGet      uint8 = 0x27
	OpMetaBatchInodeGet uint8 = 0x28
	OpMetaExtentsAdd    uint8 = 0x29
	OpMetaExtentsDel    uint8 = 0x2A
	OpMetaExtentsList   uint8 = 0x2B
	OpMetaUpdateDentry  uint8 = 0x2C
	OpMetaTruncate      uint8 = 0x2D
	OpMetaLinkInode     uint8 = 0x2E
	OpMetaEvictInode    uint8 = 0x2F
	OpMetaSetattr       uint8 = 0x30

	//Operations: MetaNode Leader -> MetaNode Follower
	OpMetaFreeInodesOnRaftFollower uint8 = 0x32

	OpMetaDeleteInode        uint8 = 0x33 // delete specified inode immediately and do not remove data.
	OpMetaBatchExtentsAdd    uint8 = 0x34 // for extents batch attachment
	OpMetaSetXAttr           uint8 = 0x35
	OpMetaGetXAttr           uint8 = 0x36
	OpMetaRemoveXAttr        uint8 = 0x37
	OpMetaListXAttr          uint8 = 0x38
	OpMetaBatchGetXAttr      uint8 = 0x39
	OpMetaExtentAddWithCheck uint8 = 0x3A // Append extent key with discard extents check
	OpMetaReadDirLimit       uint8 = 0x3D

	// Operations: Master -> MetaNode
	OpCreateMetaPartition           uint8 = 0x40
	OpMetaNodeHeartbeat             uint8 = 0x41
	OpDeleteMetaPartition           uint8 = 0x42
	OpUpdateMetaPartition           uint8 = 0x43
	OpLoadMetaPartition             uint8 = 0x44
	OpDecommissionMetaPartition     uint8 = 0x45
	OpAddMetaPartitionRaftMember    uint8 = 0x46
	OpRemoveMetaPartitionRaftMember uint8 = 0x47
	OpMetaPartitionTryToLeader      uint8 = 0x48

	// Quota
	OpMasterSetInodeQuota       uint8 = 0x50
	OpMetaBatchSetInodeQuota    uint8 = 0x51
	OpMasterDeleteInodeQuota    uint8 = 0x52
	OpMetaBatchDeleteInodeQuota uint8 = 0x53
	OpMetaGetInodeQuota         uint8 = 0x54

	// Operations: Master -> LcNode

	OpLcNodeHeartbeat      uint8 = 0x55
	OpLcNodeScan           uint8 = 0x56
	OpLcNodeSnapshotVerDel uint8 = 0x57

	// Operations: Master -> DataNode
	OpCreateDataPartition           uint8 = 0x60
	OpDeleteDataPartition           uint8 = 0x61
	OpLoadDataPartition             uint8 = 0x62
	OpDataNodeHeartbeat             uint8 = 0x63
	OpReplicateFile                 uint8 = 0x64
	OpDeleteFile                    uint8 = 0x65
	OpDecommissionDataPartition     uint8 = 0x66
	OpAddDataPartitionRaftMember    uint8 = 0x67
	OpRemoveDataPartitionRaftMember uint8 = 0x68
	OpDataPartitionTryToLeader      uint8 = 0x69
	OpQos                           uint8 = 0x6A

	// Operations: MultipartInfo
	OpCreateMultipart  uint8 = 0x70
	OpGetMultipart     uint8 = 0x71
	OpAddMultipartPart uint8 = 0x72
	OpRemoveMultipart  uint8 = 0x73
	OpListMultiparts   uint8 = 0x74

	OpBatchDeleteExtent   uint8 = 0x75 // SDK to MetaNode
	OpGetExpiredMultipart uint8 = 0x76

	//Operations: MetaNode Leader -> MetaNode Follower
	OpMetaBatchDeleteInode  uint8 = 0x90
	OpMetaBatchDeleteDentry uint8 = 0x91 // obsolete
	OpMetaBatchUnlinkInode  uint8 = 0x92 // obsolete
	OpMetaBatchEvictInode   uint8 = 0x93

	//Transaction Operations: Client -> MetaNode.
	OpMetaTxCreateInode  uint8 = 0xA0
	OpMetaTxUnlinkInode  uint8 = 0xA1
	OpMetaTxCreateDentry uint8 = 0xA2
	OpTxCommit           uint8 = 0xA3
	OpTxInodeCommit      uint8 = 0xA4
	OpTxDentryCommit     uint8 = 0xA5
	OpTxRollback         uint8 = 0xA6
	OpTxInodeRollback    uint8 = 0xA7
	OpTxDentryRollback   uint8 = 0xA8
	OpMetaTxDeleteDentry uint8 = 0xA9
	OpMetaTxUpdateDentry uint8 = 0xAA
	OpMetaTxLinkInode    uint8 = 0xAB

	//Multi version snapshot
	OpRandomWriteAppend     uint8 = 0xB1
	OpSyncRandomWriteAppend uint8 = 0xB2
	OpRandomWriteVer        uint8 = 0xB3
	OpSyncRandomWriteVer    uint8 = 0xB4
	OpSyncRandomWriteVerRsp uint8 = 0xB5
	OpTryWriteAppend        uint8 = 0xB6
	OpSyncTryWriteAppend    uint8 = 0xB7

	// Commons
	OpNoSpaceErr uint8 = 0xEE
	OpDirQuota   uint8 = 0xF1

	// Commons

	OpConflictExtentsErr uint8 = 0xF2
	OpIntraGroupNetErr   uint8 = 0xF3
	OpArgMismatchErr     uint8 = 0xF4
	OpNotExistErr        uint8 = 0xF5
	OpDiskNoSpaceErr     uint8 = 0xF6
	OpDiskErr            uint8 = 0xF7
	OpErr                uint8 = 0xF8
	OpAgain              uint8 = 0xF9
	OpExistErr           uint8 = 0xFA
	OpInodeFullErr       uint8 = 0xFB
	OpTryOtherAddr       uint8 = 0xFC
	OpNotPerm            uint8 = 0xFD
	OpNotEmpty           uint8 = 0xFE
	OpOk                 uint8 = 0xF0
	OpTryOtherExtent     uint8 = 0xE0

	OpPing                  uint8 = 0xFF
	OpMetaUpdateXAttr       uint8 = 0x3B
	OpMetaReadDirOnly       uint8 = 0x3C
	OpUploadPartConflictErr uint8 = 0x3D

	// ebs obj meta
	OpMetaObjExtentAdd       uint8 = 0xDD
	OpMetaObjExtentsList     uint8 = 0xDE
	OpMetaExtentsEmpty       uint8 = 0xDF
	OpMetaBatchObjExtentsAdd uint8 = 0xD0
	OpMetaClearInodeCache    uint8 = 0xD1

	OpMetaBatchSetXAttr uint8 = 0xD2
	OpMetaGetAllXAttr   uint8 = 0xD3

	OpMetaSetInodeLock uint8 = 0xD4
	//transaction error
	OpTxInodeInfoNotExistErr  uint8 = 0xE0
	OpTxConflictErr           uint8 = 0xE1
	OpTxDentryInfoNotExistErr uint8 = 0xE2
	OpTxRbInodeNotExistErr    uint8 = 0xE3
	OpTxRbDentryNotExistErr   uint8 = 0xE4
	OpTxInfoNotExistErr       uint8 = 0xE5
	OpTxInternalErr           uint8 = 0xE6
	OpTxCommitItemErr         uint8 = 0xE7
	OpTxRollbackItemErr       uint8 = 0xE8
	OpTxRollbackUnknownRbType uint8 = 0xE9
	OpTxTimeoutErr            uint8 = 0xEA
	OpTxSetStateErr           uint8 = 0xEB

	// multiVersion to dp/mp
	OpVersionOperation uint8 = 0xD5
	OpSplitMarkDelete  uint8 = 0xD6
)

const (
	WriteDeadlineTime                         = 5
	ReadDeadlineTime                          = 5
	SyncSendTaskDeadlineTime                  = 20
	NoReadDeadlineTime                        = -1
	BatchDeleteExtentReadDeadLineTime         = 120
	GetAllWatermarksDeadLineTime              = 60
	DefaultClusterLoadFactor          float64 = 10
	MultiVersionFlag                          = 0x80
)

// multi version operation
const (
	CreateVersion        = 1
	DeleteVersion        = 2
	CreateVersionPrepare = 3
	CreateVersionCommit  = 4
)

// stage of version building
const (
	VersionInit            = 0
	VersionWorking         = 1
	VersionWorkingTimeOut  = 2
	VersionWorkingAbnormal = 3
	VersionWorkingFinished = 4
)

// status of version
const (
	VersionNormal         = 1
	VersionDeleted        = 2
	VersionDeleting       = 3
	VersionDeleteAbnormal = 4
)

const (
	TinyExtentType   = 0
	NormalExtentType = 1
)

const (
	NormalCreateDataPartition         = 0
	DecommissionedCreateDataPartition = 1
)

// Packet defines the packet structure.
type Packet struct {
	Magic              uint8
	ExtentType         uint8 // the highest bit be set while rsp to client if version not consistent then Verseq be valid
	Opcode             uint8
	ResultCode         uint8
	RemainingFollowers uint8
	CRC                uint32
	Size               uint32
	ArgLen             uint32
	KernelOffset       uint64
	PartitionID        uint64
	ExtentID           uint64
	ExtentOffset       int64
	ReqID              int64
	Arg                []byte // for create or append ops, the data contains the address
	Data               []byte
	StartT             int64
	mesg               string
	HasPrepare         bool
	VerSeq             uint64 // only used in mod request to datanode
	DirVerList         []*VersionInfo
}

// NewPacket returns a new packet.
func NewPacket() *Packet {
	p := new(Packet)
	p.Magic = ProtoMagic
	p.StartT = time.Now().UnixNano()
	return p
}

// NewPacketReqID returns a new packet with ReqID assigned.
func NewPacketReqID() *Packet {
	p := NewPacket()
	p.ReqID = GenerateRequestID()
	return p
}

func (p *Packet) GetCopy() *Packet {
	newPacket := NewPacket()
	newPacket.ReqID = p.ReqID
	newPacket.Opcode = p.Opcode
	newPacket.PartitionID = p.PartitionID

	newPacket.Data = make([]byte, p.Size)
	copy(newPacket.Data[:p.Size], p.Data)

	newPacket.Size = p.Size
	return newPacket
}

func (p *Packet) String() string {
	return fmt.Sprintf("ReqID(%v)Op(%v)PartitionID(%v)ResultCode(%v)ExID(%v)ExtOffset(%v)KernelOff(%v)Type(%v)Seq(%v)",
		p.ReqID, p.GetOpMsg(), p.PartitionID, p.GetResultMsg(), p.ExtentID, p.ExtentOffset, p.KernelOffset, p.ExtentType, p.VerSeq)
}

// GetStoreType returns the store type.
func (p *Packet) GetStoreType() (m string) {
	switch p.ExtentType {
	case TinyExtentType:
		m = "TinyExtent"
	case NormalExtentType:
		m = "NormalExtent"
	default:
		m = "Unknown"
	}
	return
}

func (p *Packet) GetOpMsgWithReqAndResult() (m string) {
	return fmt.Sprintf("Req(%v)_(%v)_Result(%v)", p.ReqID, p.GetOpMsg(), p.GetResultMsg())
}

// GetOpMsg returns the operation type.
func (p *Packet) GetOpMsg() (m string) {
	switch p.Opcode {
	case OpCreateExtent:
		m = "OpCreateExtent"
	case OpMarkDelete:
		m = "OpMarkDelete"
	case OpSplitMarkDelete:
		m = "OpMarkDelete"
	case OpWrite:
		m = "OpWrite"
	case OpTryWriteAppend:
		m = "OpTryWriteAppend"
	case OpRandomWrite:
		m = "OpRandomWrite"
	case OpRandomWriteAppend:
		m = "OpRandomWriteAppend"
	case OpRandomWriteVer:
		m = "OpRandomWriteVer"
	case OpRead:
		m = "Read"
	case OpStreamRead:
		m = "OpStreamRead"
	case OpStreamFollowerRead:
		m = "OpStreamFollowerRead"
	case OpGetAllWatermarks:
		m = "OpGetAllWatermarks"
	case OpNotifyReplicasToRepair:
		m = "OpNotifyReplicasToRepair"
	case OpExtentRepairRead:
		m = "OpExtentRepairRead"
	case OpConflictExtentsErr:
		m = "ConflictExtentsErr"
	case OpIntraGroupNetErr:
		m = "IntraGroupNetErr"
	case OpMetaCreateInode:
		m = "OpMetaCreateInode"
	case OpMetaUnlinkInode:
		m = "OpMetaUnlinkInode"
	case OpMetaBatchUnlinkInode:
		m = "OpMetaBatchUnlinkInode"
	case OpMetaCreateDentry:
		m = "OpMetaCreateDentry"
	case OpMetaDeleteDentry:
		m = "OpMetaDeleteDentry"
	case OpMetaBatchDeleteDentry:
		m = "OpMetaBatchDeleteDentry"
	case OpMetaOpen:
		m = "OpMetaOpen"
	case OpMetaLookup:
		m = "OpMetaLookup"
	case OpMetaReadDir:
		m = "OpMetaReadDir"
	case OpMetaReadDirLimit:
		m = "OpMetaReadDirLimit"
	case OpMetaInodeGet:
		m = "OpMetaInodeGet"
	case OpMetaBatchInodeGet:
		m = "OpMetaBatchInodeGet"
	case OpMetaExtentsAdd:
		m = "OpMetaExtentsAdd"
	case OpMetaExtentAddWithCheck:
		m = "OpMetaExtentAddWithCheck"
	case OpMetaObjExtentAdd:
		m = "OpMetaObjExtentAdd"
	case OpMetaExtentsDel:
		m = "OpMetaExtentsDel"
	case OpMetaExtentsList:
		m = "OpMetaExtentsList"
	case OpMetaObjExtentsList:
		m = "OpMetaObjExtentsList"
	case OpMetaUpdateDentry:
		m = "OpMetaUpdateDentry"
	case OpMetaTruncate:
		m = "OpMetaTruncate"
	case OpMetaLinkInode:
		m = "OpMetaLinkInode"
	case OpMetaEvictInode:
		m = "OpMetaEvictInode"
	case OpMetaBatchEvictInode:
		m = "OpMetaBatchEvictInode"
	case OpMetaSetattr:
		m = "OpMetaSetattr"
	case OpCreateMetaPartition:
		m = "OpCreateMetaPartition"
	case OpMetaNodeHeartbeat:
		m = "OpMetaNodeHeartbeat"
	case OpDeleteMetaPartition:
		m = "OpDeleteMetaPartition"
	case OpUpdateMetaPartition:
		m = "OpUpdateMetaPartition"
	case OpLoadMetaPartition:
		m = "OpLoadMetaPartition"
	case OpDecommissionMetaPartition:
		m = "OpDecommissionMetaPartition"
	case OpCreateDataPartition:
		m = "OpCreateDataPartition"
	case OpDeleteDataPartition:
		m = "OpDeleteDataPartition"
	case OpLoadDataPartition:
		m = "OpLoadDataPartition"
	case OpDecommissionDataPartition:
		m = "OpDecommissionDataPartition"
	case OpDataNodeHeartbeat:
		m = "OpDataNodeHeartbeat"
	case OpReplicateFile:
		m = "OpReplicateFile"
	case OpDeleteFile:
		m = "OpDeleteFile"
	case OpGetAppliedId:
		m = "OpGetAppliedId"
	case OpGetPartitionSize:
		m = "OpGetPartitionSize"
	case OpSyncWrite:
		m = "OpSyncWrite"
	case OpSyncTryWriteAppend:
		m = "OpSyncTryWriteAppend"
	case OpSyncRandomWrite:
		m = "OpSyncRandomWrite"
	case OpSyncRandomWriteVer:
		m = "OpSyncRandomWriteVer"
	case OpSyncRandomWriteAppend:
		m = "OpSyncRandomWriteAppend"
	case OpReadTinyDeleteRecord:
		m = "OpReadTinyDeleteRecord"
	case OpPing:
		m = "OpPing"
	case OpTinyExtentRepairRead:
		m = "OpTinyExtentRepairRead"
	case OpGetMaxExtentIDAndPartitionSize:
		m = "OpGetMaxExtentIDAndPartitionSize"
	case OpBroadcastMinAppliedID:
		m = "OpBroadcastMinAppliedID"
	case OpRemoveDataPartitionRaftMember:
		m = "OpRemoveDataPartitionRaftMember"
	case OpAddDataPartitionRaftMember:
		m = "OpAddDataPartitionRaftMember"
	case OpAddMetaPartitionRaftMember:
		m = "OpAddMetaPartitionRaftMember"
	case OpRemoveMetaPartitionRaftMember:
		m = "OpRemoveMetaPartitionRaftMember"
	case OpMetaPartitionTryToLeader:
		m = "OpMetaPartitionTryToLeader"
	case OpDataPartitionTryToLeader:
		m = "OpDataPartitionTryToLeader"
	case OpMetaDeleteInode:
		m = "OpMetaDeleteInode"
	case OpMetaBatchDeleteInode:
		m = "OpMetaBatchDeleteInode"
	case OpMetaBatchExtentsAdd:
		m = "OpMetaBatchExtentsAdd"
	case OpMetaBatchObjExtentsAdd:
		m = "OpMetaBatchObjExtentsAdd"
	case OpMetaSetXAttr:
		m = "OpMetaSetXAttr"
	case OpMetaGetXAttr:
		m = "OpMetaGetXAttr"
	case OpMetaRemoveXAttr:
		m = "OpMetaRemoveXAttr"
	case OpMetaListXAttr:
		m = "OpMetaListXAttr"
	case OpMetaBatchGetXAttr:
		m = "OpMetaBatchGetXAttr"
	case OpMetaUpdateXAttr:
		m = "OpMetaUpdateXAttr"
	case OpCreateMultipart:
		m = "OpCreateMultipart"
	case OpGetMultipart:
		m = "OpGetMultipart"
	case OpAddMultipartPart:
		m = "OpAddMultipartPart"
	case OpRemoveMultipart:
		m = "OpRemoveMultipart"
	case OpListMultiparts:
		m = "OpListMultiparts"
	case OpBatchDeleteExtent:
		m = "OpBatchDeleteExtent"
	case OpMetaClearInodeCache:
		m = "OpMetaClearInodeCache"
	case OpMetaTxCreateInode:
		m = "OpMetaTxCreateInode"
	case OpMetaTxCreateDentry:
		m = "OpMetaTxCreateDentry"
	case OpTxCommit:
		m = "OpTxCommit"
	case OpTxInodeCommit:
		m = "OpTxInodeCommit"
	case OpTxDentryCommit:
		m = "OpTxDentryCommit"
	case OpTxRollback:
		m = "OpTxRollback"
	case OpTxInodeRollback:
		m = "OpTxInodeRollback"
	case OpTxDentryRollback:
		m = "OpTxDentryRollback"
	case OpMetaTxDeleteDentry:
		m = "OpMetaTxDeleteDentry"
	case OpMetaTxUnlinkInode:
		m = "OpMetaTxUnlinkInode"
	case OpMetaTxUpdateDentry:
		m = "OpMetaTxUpdateDentry"
	case OpMetaTxLinkInode:
		m = "OpMetaTxLinkInode"
	case OpMasterSetInodeQuota:
		m = "OpMasterSetInodeQuota"
	case OpMasterDeleteInodeQuota:
		m = "OpMasterDeleteInodeQuota"
	case OpMetaBatchSetInodeQuota:
		m = "OpMetaBatchSetInodeQuota"
	case OpMetaBatchDeleteInodeQuota:
		m = "OpMetaBatchDeleteInodeQuota"
	case OpMetaGetInodeQuota:
		m = "OpMetaGetInodeQuota"
	}
	return
}

// GetResultMsg returns the result message.
func (p *Packet) GetResultMsg() (m string) {
	if p == nil {
		return ""
	}

	switch p.ResultCode {
	case OpConflictExtentsErr:
		m = "ConflictExtentsErr"
	case OpIntraGroupNetErr:
		m = "IntraGroupNetErr"
	case OpDiskNoSpaceErr:
		m = "DiskNoSpaceErr"
	case OpDiskErr:
		m = "DiskErr"
	case OpErr:
		m = "Err: " + string(p.Data)
	case OpAgain:
		m = "Again: " + string(p.Data)
	case OpOk:
		m = "Ok"
	case OpExistErr:
		m = "ExistErr"
	case OpInodeFullErr:
		m = "InodeFullErr"
	case OpArgMismatchErr:
		m = "ArgUnmatchErr"
	case OpNotExistErr:
		m = "NotExistErr"
	case OpTryOtherAddr:
		m = "TryOtherAddr"
	case OpNotPerm:
		m = "NotPerm"
	case OpNotEmpty:
		m = "DirNotEmpty"
	case OpDirQuota:
		m = "OpDirQuota"
	case OpNoSpaceErr:
		m = "NoSpaceErr"
	case OpTxInodeInfoNotExistErr:
		m = "OpTxInodeInfoNotExistErr"
	case OpTxConflictErr:
		m = "TransactionConflict"
	case OpTxDentryInfoNotExistErr:
		m = "OpTxDentryInfoNotExistErr"
	case OpTxRbInodeNotExistErr:
		m = "OpTxRbInodeNotExistEr"
	case OpTxRbDentryNotExistErr:
		m = "OpTxRbDentryNotExistEr"
	case OpTxInfoNotExistErr:
		m = "OpTxInfoNotExistErr"
	case OpTxInternalErr:
		m = "OpTxInternalErr"
	case OpTxCommitItemErr:
		m = "OpTxCommitItemErr"
	case OpTxRollbackItemErr:
		m = "OpTxRollbackItemErr"
	case OpTxRollbackUnknownRbType:
		m = "OpTxRollbackUnknownRbType"
	case OpTxTimeoutErr:
		m = "OpTxTimeoutErr"
	case OpTxSetStateErr:
		m = "OpTxSetStateErr"
	case OpUploadPartConflictErr:
		m = "OpUploadPartConflictErr"
	default:
		return fmt.Sprintf("Unknown ResultCode(%v)", p.ResultCode)
	}
	return
}

func (p *Packet) GetReqID() int64 {
	return p.ReqID
}

// MarshalHeader marshals the packet header.
func (p *Packet) MarshalHeader(out []byte) {
	out[0] = p.Magic
	out[1] = p.ExtentType
	out[2] = p.Opcode
	out[3] = p.ResultCode
	out[4] = p.RemainingFollowers
	binary.BigEndian.PutUint32(out[5:9], p.CRC)
	binary.BigEndian.PutUint32(out[9:13], p.Size)
	binary.BigEndian.PutUint32(out[13:17], p.ArgLen)
	binary.BigEndian.PutUint64(out[17:25], p.PartitionID)
	binary.BigEndian.PutUint64(out[25:33], p.ExtentID)
	binary.BigEndian.PutUint64(out[33:41], uint64(p.ExtentOffset))
	binary.BigEndian.PutUint64(out[41:49], uint64(p.ReqID))
	binary.BigEndian.PutUint64(out[49:util.PacketHeaderSize], p.KernelOffset)
	if p.Opcode == OpRandomWriteVer || p.ExtentType&MultiVersionFlag > 0 {
		binary.BigEndian.PutUint64(out[util.PacketHeaderSize:util.PacketHeaderSize+8], p.VerSeq)
	}
	return
}

// UnmarshalHeader unmarshals the packet header.
func (p *Packet) UnmarshalHeader(in []byte) error {
	p.Magic = in[0]
	if p.Magic != ProtoMagic {
		return errors.New("Bad Magic " + strconv.Itoa(int(p.Magic)))
	}

	p.ExtentType = in[1]
	p.Opcode = in[2]
	p.ResultCode = in[3]
	p.RemainingFollowers = in[4]
	p.CRC = binary.BigEndian.Uint32(in[5:9])
	p.Size = binary.BigEndian.Uint32(in[9:13])
	p.ArgLen = binary.BigEndian.Uint32(in[13:17])
	p.PartitionID = binary.BigEndian.Uint64(in[17:25])
	p.ExtentID = binary.BigEndian.Uint64(in[25:33])
	p.ExtentOffset = int64(binary.BigEndian.Uint64(in[33:41]))
	p.ReqID = int64(binary.BigEndian.Uint64(in[41:49]))
	p.KernelOffset = binary.BigEndian.Uint64(in[49:util.PacketHeaderSize])

	// header opcode OpRandomWriteVer should not unmarshal here due to header size is const
	// the ver param should read at the higher level directly
	//if p.Opcode ==OpRandomWriteVer {

	return nil
}

// MarshalData marshals the packet data.
func (p *Packet) MarshalData(v interface{}) error {
	data, err := json.Marshal(v)
	if err == nil {
		p.Data = data
		p.Size = uint32(len(p.Data))
	}
	return err
}

// UnmarshalData unmarshals the packet data.
func (p *Packet) UnmarshalData(v interface{}) error {
	return json.Unmarshal(p.Data, v)
}

// WriteToNoDeadLineConn writes through the connection without deadline.
func (p *Packet) WriteToNoDeadLineConn(c net.Conn) (err error) {
	header, err := Buffers.Get(util.PacketHeaderSize)
	if err != nil {
		header = make([]byte, util.PacketHeaderSize)
	}
	defer Buffers.Put(header)

	p.MarshalHeader(header)
	if _, err = c.Write(header); err == nil {
		if _, err = c.Write(p.Arg[:int(p.ArgLen)]); err == nil {
			if p.Data != nil {
				_, err = c.Write(p.Data[:p.Size])
			}
		}
	}

	return
}

// WriteToConn writes through the given connection.
func (p *Packet) WriteToConn(c net.Conn) (err error) {
	headSize := util.PacketHeaderSize
	if p.Opcode == OpRandomWriteVer || p.ExtentType&MultiVersionFlag > 0 {
		headSize = util.PacketHeaderVerSize
	}
	log.LogDebugf("packet opcode %v header size %v extentype %v conn %v", p.Opcode, headSize, p.ExtentType, c)
	header, err := Buffers.Get(headSize)
	if err != nil {
		header = make([]byte, headSize)
	}
	// log.LogErrorf("action[WriteToConn] buffer get nil,opcode %v head len [%v]", p.Opcode, len(header))
	defer Buffers.Put(header)
	c.SetWriteDeadline(time.Now().Add(WriteDeadlineTime * time.Second))
	p.MarshalHeader(header)
	if _, err = c.Write(header); err == nil {
		log.LogDebugf("packet opcode %v header size %v extentype %v size %v", p.Opcode, headSize, p.ExtentType, len(header))
		if _, err = c.Write(p.Arg[:int(p.ArgLen)]); err == nil {
			log.LogDebugf("packet opcode %v header size %v extentype %v arg size %v", p.Opcode, headSize, p.ExtentType, p.ArgLen)
			if p.Data != nil && p.Size != 0 {
				_, err = c.Write(p.Data[:p.Size])
				log.LogDebugf("packet opcode %v header size %v extentype %v data size %v", p.Opcode, headSize, p.ExtentType, p.Size)
			}
		}
	}

	return
}

// ReadFull is a wrapper function of io.ReadFull.
func ReadFull(c net.Conn, buf *[]byte, readSize int) (err error) {
	*buf = make([]byte, readSize)
	_, err = io.ReadFull(c, (*buf)[:readSize])
	return
}

// ReadFromConn reads the data from the given connection.
// Recognize the version bit and parse out version,
// to avoid version field rsp back , the rsp of random write from datanode with replace OpRandomWriteVer to OpRandomWriteVerRsp
func (p *Packet) ReadFromConnWithVer(c net.Conn, timeoutSec int) (err error) {
	if timeoutSec != NoReadDeadlineTime {
		c.SetReadDeadline(time.Now().Add(time.Second * time.Duration(timeoutSec)))
	} else {
		c.SetReadDeadline(time.Time{})
	}

	header, err := Buffers.Get(util.PacketHeaderSize)
	if err != nil {
		header = make([]byte, util.PacketHeaderSize)
	}
	defer Buffers.Put(header)
	var n int
	if n, err = io.ReadFull(c, header); err != nil {
		log.LogErrorf("ReadFromConnWithVer. read header fail %v", err)
		return
	}
	if n != util.PacketHeaderSize {
		return syscall.EBADMSG
	}
	if err = p.UnmarshalHeader(header); err != nil {
		return
	}

	log.LogDebugf("action[ReadFromConnWithVer] verseq %v", p.VerSeq)

	if p.ExtentType&MultiVersionFlag > 0 {
		ver := make([]byte, 8)
		if _, err = io.ReadFull(c, ver); err != nil {
			return
		}
		p.VerSeq = binary.BigEndian.Uint64(ver)
	}

	if p.ArgLen > 0 {
		p.Arg = make([]byte, int(p.ArgLen))
		if _, err = io.ReadFull(c, p.Arg[:int(p.ArgLen)]); err != nil {
			return err
		}
	}

	if p.Size < 0 {
		return syscall.EBADMSG
	}
	size := p.Size
	if (p.Opcode == OpRead || p.Opcode == OpStreamRead || p.Opcode == OpExtentRepairRead || p.Opcode == OpStreamFollowerRead) && p.ResultCode == OpInitResultCode {
		size = 0
	}
	p.Data = make([]byte, size)
	if n, err = io.ReadFull(c, p.Data[:size]); err != nil {
		return err
	}
	if n != int(size) {
		return syscall.EBADMSG
	}
	return nil
}

// ReadFromConn reads the data from the given connection.
func (p *Packet) ReadFromConn(c net.Conn, timeoutSec int) (err error) {
	if timeoutSec != NoReadDeadlineTime {
		c.SetReadDeadline(time.Now().Add(time.Second * time.Duration(timeoutSec)))
	} else {
		c.SetReadDeadline(time.Time{})
	}
	header, err := Buffers.Get(util.PacketHeaderSize)
	if err != nil {
		header = make([]byte, util.PacketHeaderSize)
	}
	defer Buffers.Put(header)
	var n int
	if n, err = io.ReadFull(c, header); err != nil {
		return
	}
	if n != util.PacketHeaderSize {
		return syscall.EBADMSG
	}
	if err = p.UnmarshalHeader(header); err != nil {
		return
	}

	if p.ArgLen > 0 {
		p.Arg = make([]byte, int(p.ArgLen))
		if _, err = io.ReadFull(c, p.Arg[:int(p.ArgLen)]); err != nil {
			return err
		}
	}

	if p.Size < 0 {
		return syscall.EBADMSG
	}
	size := p.Size
	if (p.Opcode == OpRead || p.Opcode == OpStreamRead || p.Opcode == OpExtentRepairRead || p.Opcode == OpStreamFollowerRead) && p.ResultCode == OpInitResultCode {
		size = 0
	}
	p.Data = make([]byte, size)
	if n, err = io.ReadFull(c, p.Data[:size]); err != nil {
		return err
	}
	if n != int(size) {
		return syscall.EBADMSG
	}
	return nil
}

// PacketOkReply sets the result code as OpOk, and sets the body as empty.
func (p *Packet) PacketOkReply() {
	p.ResultCode = OpOk
	p.Size = 0
	p.Data = nil
	p.ArgLen = 0
}

// PacketOkWithBody sets the result code as OpOk, and sets the body with the give data.
func (p *Packet) PacketOkWithBody(reply []byte) {
	p.Size = uint32(len(reply))
	p.Data = make([]byte, p.Size)
	copy(p.Data[:p.Size], reply)
	p.ResultCode = OpOk
	p.ArgLen = 0
}

// attention use for tmp byte arr, eg: json marshal data
func (p *Packet) PacketOkWithByte(reply []byte) {
	p.Size = uint32(len(reply))
	p.Data = reply
	p.ResultCode = OpOk
	p.ArgLen = 0
}

// PacketErrorWithBody sets the packet with error code whose body is filled with the given data.
func (p *Packet) PacketErrorWithBody(code uint8, reply []byte) {
	p.Size = uint32(len(reply))
	p.Data = make([]byte, p.Size)
	copy(p.Data[:p.Size], reply)
	p.ResultCode = code
	p.ArgLen = 0
}

func (p *Packet) SetPacketHasPrepare() {
	p.setPacketPrefix()
	p.HasPrepare = true
}

func (p *Packet) SetPacketRePrepare() {
	p.HasPrepare = false
}

func (p *Packet) AddMesgLog(m string) {
	p.mesg += m
}

// GetUniqueLogId returns the unique log ID.
func (p *Packet) GetUniqueLogId() (m string) {
	defer func() {
		m = m + fmt.Sprintf("_ResultMesg(%v)", p.GetResultMsg())
	}()
	if p.HasPrepare {
		m = p.mesg
		return
	}
	m = fmt.Sprintf("Req(%v)_Partition(%v)_", p.ReqID, p.PartitionID)
	if (p.Opcode == OpSplitMarkDelete || (p.ExtentType == TinyExtentType && p.Opcode == OpMarkDelete)) && len(p.Data) > 0 {
		ext := new(TinyExtentDeleteRecord)
		err := json.Unmarshal(p.Data, ext)
		if err == nil {
			m += fmt.Sprintf("Extent(%v)_ExtentOffset(%v)_Size(%v)_Opcode(%v)",
				ext.ExtentId, ext.ExtentOffset, ext.Size, p.GetOpMsg())
			return m
		}
	} else if p.Opcode == OpReadTinyDeleteRecord || p.Opcode == OpNotifyReplicasToRepair || p.Opcode == OpDataNodeHeartbeat ||
		p.Opcode == OpLoadDataPartition || p.Opcode == OpBatchDeleteExtent {
		p.mesg += fmt.Sprintf("Opcode(%v)", p.GetOpMsg())
		return
	} else if p.Opcode == OpBroadcastMinAppliedID || p.Opcode == OpGetAppliedId {
		if p.Size > 0 {
			applyID := binary.BigEndian.Uint64(p.Data)
			m += fmt.Sprintf("Opcode(%v)_AppliedID(%v)", p.GetOpMsg(), applyID)
		} else {
			m += fmt.Sprintf("Opcode(%v)", p.GetOpMsg())
		}
		return m
	}
	m = fmt.Sprintf("Req(%v)_Partition(%v)_Extent(%v)_ExtentOffset(%v)_KernelOffset(%v)_"+
		"Size(%v)_Opcode(%v)_CRC(%v)",
		p.ReqID, p.PartitionID, p.ExtentID, p.ExtentOffset,
		p.KernelOffset, p.Size, p.GetOpMsg(), p.CRC)

	return
}

func (p *Packet) setPacketPrefix() {
	p.mesg = fmt.Sprintf("Req(%v)_Partition(%v)_", p.ReqID, p.PartitionID)
	if (p.Opcode == OpSplitMarkDelete || (p.ExtentType == TinyExtentType && p.Opcode == OpMarkDelete)) && len(p.Data) > 0 {
		ext := new(TinyExtentDeleteRecord)
		err := json.Unmarshal(p.Data, ext)
		if err == nil {
			p.mesg += fmt.Sprintf("Extent(%v)_ExtentOffset(%v)_Size(%v)_Opcode(%v)",
				ext.ExtentId, ext.ExtentOffset, ext.Size, p.GetOpMsg())
			return
		}
	} else if p.Opcode == OpReadTinyDeleteRecord || p.Opcode == OpNotifyReplicasToRepair || p.Opcode == OpDataNodeHeartbeat ||
		p.Opcode == OpLoadDataPartition || p.Opcode == OpBatchDeleteExtent {
		p.mesg += fmt.Sprintf("Opcode(%v)", p.GetOpMsg())
		return
	} else if p.Opcode == OpBroadcastMinAppliedID || p.Opcode == OpGetAppliedId {
		if p.Size > 0 {
			applyID := binary.BigEndian.Uint64(p.Data)
			p.mesg += fmt.Sprintf("Opcode(%v)_AppliedID(%v)", p.GetOpMsg(), applyID)
		} else {
			p.mesg += fmt.Sprintf("Opcode(%v)", p.GetOpMsg())
		}
		return
	}
	p.mesg = fmt.Sprintf("Req(%v)_Partition(%v)_Extent(%v)_ExtentOffset(%v)_KernelOffset(%v)_"+
		"Size(%v)_Opcode(%v)_CRC(%v)",
		p.ReqID, p.PartitionID, p.ExtentID, p.ExtentOffset,
		p.KernelOffset, p.Size, p.GetOpMsg(), p.CRC)

}

// IsForwardPkt returns if the packet is the forward packet (a packet that will be forwarded to the followers).
func (p *Packet) IsDirSnapshotOperate() bool {
	if p.VerSeq > 0 && len(p.DirVerList) > 0 {
		return true
	}
	return false
}

// IsForwardPkt returns if the packet is the forward packet (a packet that will be forwarded to the followers).
func (p *Packet) IsForwardPkt() bool {
	return p.RemainingFollowers > 0
}

// LogMessage logs the given message.
func (p *Packet) LogMessage(action, remote string, start int64, err error) (m string) {
	if err == nil {
		m = fmt.Sprintf("id[%v] isPrimaryBackReplLeader[%v] remote[%v] "+
			" cost[%v] ", p.GetUniqueLogId(), p.IsForwardPkt(), remote, (time.Now().UnixNano()-start)/1e6)

	} else {
		m = fmt.Sprintf("id[%v] isPrimaryBackReplLeader[%v] remote[%v]"+
			", err[%v]", p.GetUniqueLogId(), p.IsForwardPkt(), remote, err.Error())
	}

	return
}

// ShallRetry returns if we should retry the packet.
func (p *Packet) ShouldRetry() bool {
	return p.ResultCode == OpAgain || p.ResultCode == OpErr
}

func (p *Packet) IsBatchDeleteExtents() bool {
	return p.Opcode == OpBatchDeleteExtent
}

func InitBufferPool(bufLimit int64) {
	buf.NormalBuffersTotalLimit = bufLimit
	buf.HeadBuffersTotalLimit = bufLimit
	buf.HeadVerBuffersTotalLimit = bufLimit

	Buffers = buf.NewBufferPool()
}
