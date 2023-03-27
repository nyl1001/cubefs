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

package data

import (
	"context"
	"fmt"
	"sync"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/unit"
)

// request with size greater than the threshold will not read ahead
const readAheadThreshold int = 16 * 1024

// ExtentReader defines the struct of the extent reader.
type ExtentReader struct {
	inode        uint64
	key          *proto.ExtentKey
	dp           *DataPartition
	followerRead bool
	readAhead    bool
	req          *ExtentRequest
	reqMutex     sync.Mutex
}

// NewExtentReader returns a new extent reader.
func NewExtentReader(inode uint64, key *proto.ExtentKey, dp *DataPartition, followerRead bool, readAhead bool) *ExtentReader {
	return &ExtentReader{
		inode:        inode,
		key:          key,
		dp:           dp,
		followerRead: followerRead,
		readAhead:    readAhead,
	}
}

// String returns the string format of the extent reader.
func (er *ExtentReader) String() (m string) {
	if er == nil {
		return ""
	}
	return fmt.Sprintf("inode (%v) extentKey(%v)", er.inode,
		er.key.Marshal())
}

func (er *ExtentReader) EcTinyExtentRead(ctx context.Context, req *ExtentRequest) (readBytes int, err error) {
	offset := int(req.FileOffset) - int(er.key.FileOffset) + int(er.key.ExtentOffset)
	size := req.Size

	reqPacket := NewTinyExtentReadPacket(ctx, req.ExtentKey.PartitionId, req.ExtentKey.ExtentId, offset, req.Size)

	log.LogDebugf("grep : size(%v) req(%v) reqPacket(%v)", size, req, reqPacket)

	readBytes, err = er.read(er.dp, reqPacket, req, er.followerRead)

	if err != nil {
		log.LogWarnf("ExtentReader EcTinyExtentRead: err(%v) req(%v) reqPacket(%v)", err, req, reqPacket)
	}

	log.LogDebugf("ExtentReader EcTinyExtentRead exit: req(%v) reqPacket(%v) readBytes(%v) err(%v)", req, reqPacket, readBytes, err)
	return
}

// Read reads the extent request.
func (er *ExtentReader) Read(ctx context.Context, req *ExtentRequest) (readBytes int, err error) {
	// read from read ahead cache
	if er.readAhead {
		er.reqMutex.Lock()
		defer er.reqMutex.Unlock()
		if er.req != nil && req.FileOffset >= er.req.FileOffset && req.FileOffset+uint64(req.Size) <= er.req.FileOffset+uint64(er.req.Size) {
			copy(req.Data[0:req.Size], er.req.Data[req.FileOffset-er.req.FileOffset:req.FileOffset-er.req.FileOffset+uint64(req.Size)])
			readBytes = req.Size
			return
		}
	}

	offset := int(req.FileOffset - er.key.FileOffset + er.key.ExtentOffset)
	size := req.Size

	readAhead := false
	realReq := req
	if er.readAhead && size <= readAheadThreshold &&
		uint64(req.FileOffset)-req.ExtentKey.FileOffset+req.ExtentKey.ExtentOffset+uint64(unit.BlockSize) <= uint64(req.ExtentKey.Size) {
		readAhead = true
		size = unit.BlockSize
		data := make([]byte, size)
		realReq = NewExtentRequest(req.FileOffset, size, data, req.ExtentKey)
	}

	reqPacket := NewReadPacket(ctx, er.key, offset, size, er.inode, req.FileOffset, er.followerRead)

	log.LogDebugf("ExtentReader Read enter: req.Size(%v) size(%v) req(%v) reqPacket(%v)", req.Size, size, req, reqPacket)

	readBytes, err = er.read(er.dp, reqPacket, realReq, er.followerRead)
	if readAhead {
		realReq.Size = readBytes
		if readBytes > req.Size {
			readBytes = req.Size
		}
		copy(req.Data[0:readBytes], realReq.Data[0:readBytes])
		er.req = realReq
	}

	if err != nil {
		log.LogWarnf("Extent Reader Read: err(%v) req(%v) reqPacket(%v)", err, req, reqPacket)
	}

	log.LogDebugf("ExtentReader Read exit: req.Size(%v) size(%v) req(%v) reqPacket(%v) readBytes(%v) err(%v)", req.Size, size, req, reqPacket, readBytes, err)
	return
}

func (er *ExtentReader) read(dp *DataPartition, reqPacket *Packet, req *ExtentRequest, followerRead bool) (readBytes int, err error) {
	var sc *StreamConn
	if dp.canEcRead() {
		reqPacket.Opcode = proto.OpStreamEcRead
		if sc, readBytes, err = dp.EcRead(reqPacket, req); err == nil {
			return
		}
		log.LogWarnf("read error: read EC failed, read data from replicate, err(%v)", err)
		errMsg := fmt.Sprintf("read EC failed inode(%v) req(%v)", er.inode, req)
		handleUmpAlarm(dp.ClientWrapper.clusterName, dp.ClientWrapper.volName, "ecRead", errMsg)
	}
	if !followerRead {
		sc, readBytes, err = dp.LeaderRead(reqPacket, req)
		if err != nil {
			log.LogWarnf("read error: read leader failed, err(%v)", err)
			readBytes, err = dp.ReadConsistentFromHosts(sc, reqPacket, req)
		}
	} else {
		sc, readBytes, err = dp.FollowerRead(reqPacket, req)
	}
	if err != nil {
		log.LogWarnf("read error: err(%v), followerRead(%v)", err, followerRead)
		return readBytes, err
	}
	return
}
