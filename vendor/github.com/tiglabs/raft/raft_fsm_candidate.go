// Copyright 2015 The etcd Authors
// Modified work copyright 2018 The tiglabs Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package raft

import (
	"context"
	"fmt"
	"sort"

	"github.com/tiglabs/raft/tracing"

	"github.com/tiglabs/raft/logger"
	"github.com/tiglabs/raft/proto"
)

func (r *raftFsm) becomeCandidate() {
	if r.state == stateLeader {
		panic(AppPanicError(fmt.Sprintf("[raft->becomeCandidate][%v] invalid transition [leader -> candidate].", r.id)))
	}

	r.step = stepCandidate
	r.reset(r.term+1, 0, false)
	r.tick = r.tickElection
	r.vote = r.config.NodeID
	r.state = stateCandidate

	if logger.IsEnableDebug() {
		logger.Debug("raft[%v] became candidate at term %d.", r.id, r.term)
	}
}

func stepCandidate(r *raftFsm, m *proto.Message) {
	var tracer = tracing.TracerFromContext(m.Ctx()).ChildTracer("raftFsm.stepCandidate")
	defer tracer.Finish()
	m.SetTagsToTracer(tracer)
	m.SetCtx(tracer.Context())
	switch m.Type {
	case proto.LocalMsgProp:
		if logger.IsEnableDebug() {
			logger.Debug("raft[%v] no leader at term %d; dropping proposal.", r.id, r.term)
		}
		proto.ReturnMessage(m)
		return

	case proto.ReqMsgAppend:
		r.becomeFollower(m.Ctx(), r.term, m.From)
		r.handleAppendEntries(m)
		proto.ReturnMessage(m)
		return

	case proto.ReqMsgHeartBeat:
		r.becomeFollower(m.Ctx(), r.term, m.From)
		return

	case proto.ReqMsgElectAck:
		r.becomeFollower(m.Ctx(), r.term, m.From)
		nmsg := proto.GetMessage()
		nmsg.Type = proto.RespMsgElectAck
		nmsg.To = m.From
		nmsg.SetCtx(m.Ctx())
		r.send(nmsg)
		proto.ReturnMessage(m)
		return

	case proto.ReqMsgVote:
		if logger.IsEnableDebug() {
			logger.Debug("raft[%v] [logterm: %d, index: %d, vote: %v] rejected vote from %v [logterm: %d, index: %d] at term %d.", r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.vote, m.From, m.LogTerm, m.Index, r.term)
		}
		nmsg := proto.GetMessage()
		nmsg.Type = proto.RespMsgVote
		nmsg.To = m.From
		nmsg.Reject = true
		nmsg.SetCtx(m.Ctx())
		r.send(nmsg)
		proto.ReturnMessage(m)
		return

	case proto.RespMsgVote:
		gr := r.poll(m.From, !m.Reject)
		if logger.IsEnableDebug() {
			logger.Debug("raft[%v] [q:%d] has received %d votes and %d vote rejections.", r.id, r.quorum(), gr, len(r.votes)-gr)
		}
		switch r.quorum() {
		case gr:
			if r.config.LeaseCheck {
				r.becomeElectionAck()
			} else {
				r.becomeLeader()
				r.bcastAppend(m.Ctx())
			}
		case len(r.votes) - gr:
			r.becomeFollower(m.Ctx(), r.term, NoLeader)
		}
	}
}

func (r *raftFsm) isNeedBecomeFollower() bool {
	if r.state == stateCandidate {
		peerIDs := make([]uint64, 0, len(r.replicas))
		for peerID := range r.replicas {
			peerIDs = append(peerIDs, peerID)
		}
		sort.SliceStable(peerIDs, func(i, j int) bool {
			return peerIDs[i] < peerIDs[j]
		})
		return peerIDs[int(r.term)%len(peerIDs)] == r.config.NodeID
	}
	return false
}

func (r *raftFsm) campaign(force bool) {
	if r.isNeedBecomeFollower() {
		if logger.IsEnableDebug() {
			logger.Debug("[raft->campaign][%v] need degrade to follower at term %d", r.id, r.term)
		}
		r.becomeFollower(context.Background(), r.term, NoLeader)
		return
	}

	r.becomeCandidate()
	if r.quorum() == r.poll(r.config.NodeID, true) {
		if r.config.LeaseCheck {
			r.becomeElectionAck()
		} else {
			r.becomeLeader()
		}
		return
	}

	for id := range r.replicas {
		if id == r.config.NodeID || r.replicas[id].isLearner {
			continue
		}
		li, lt := r.raftLog.lastIndexAndTerm()
		if logger.IsEnableDebug() {
			logger.Debug("[raft->campaign][%v logterm: %d, index: %d] sent vote request to %v at term %d.", r.id, lt, li, id, r.term)
		}

		m := proto.GetMessage()
		m.To = id
		m.Type = proto.ReqMsgVote
		m.ForceVote = force
		m.Index = li
		m.LogTerm = lt
		r.send(m)
	}
}

func (r *raftFsm) poll(id uint64, v bool) (granted int) {
	if logger.IsEnableDebug() {
		if v {
			logger.Debug("raft[%v] received vote from %v at term %d.", r.id, id, r.term)
		} else {
			logger.Debug("raft[%v] received vote rejection from %v at term %d.", r.id, id, r.term)
		}
	}
	if _, ok := r.votes[id]; !ok {
		r.votes[id] = v
	}
	for _, vv := range r.votes {
		if vv {
			granted++
		}
	}
	return granted
}
