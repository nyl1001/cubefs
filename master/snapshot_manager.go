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

package master

import (
	"context"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
)

type snapshotDelManager struct {
	cluster              *Cluster
	lcSnapshotTaskStatus *lcSnapshotVerStatus
	lcNodeStatus         *lcNodeStatus
	idleNodeCh           chan struct{}
	exitCh               chan struct{}
}

func newSnapshotManager(ctx context.Context) *snapshotDelManager {
	proto.SpanFromContext(ctx).Infof("action[newSnapshotManager] construct")
	snapshotMgr := &snapshotDelManager{
		lcSnapshotTaskStatus: newLcSnapshotVerStatus(),
		lcNodeStatus:         newLcNodeStatus(),
		idleNodeCh:           make(chan struct{}, 1000), // support notify multi snapshot tasks
		exitCh:               make(chan struct{}),
	}
	return snapshotMgr
}

func (m *snapshotDelManager) process(_ context.Context) {
	rCtx := proto.RoundContext("snapshot-del")
	for {
		ctx := rCtx()
		span := getSpan(ctx)
		select {
		case <-m.exitCh:
			span.Info("exitCh notified, snapshotDelManager process exit")
			return
		case <-m.idleNodeCh:
			span.Debug("idleLcNodeCh notified")

			task := m.lcSnapshotTaskStatus.GetOneTask(ctx)
			if task == nil {
				span.Debugf("lcSnapshotTaskStatus.GetOneTask, no task")
				continue
			}

			nodeAddr := m.lcNodeStatus.GetIdleNode()
			if nodeAddr == "" {
				span.Warn("no idle lcnode, redo task")
				m.lcSnapshotTaskStatus.RedoTask(task)
				continue
			}

			val, ok := m.cluster.lcNodes.Load(nodeAddr)
			if !ok {
				span.Errorf("lcNodes.Load, nodeAddr(%v) is not available, redo task", nodeAddr)
				m.lcNodeStatus.RemoveNode(nodeAddr)
				m.lcSnapshotTaskStatus.RedoTask(task)
				continue
			}

			node := val.(*LcNode)
			adminTask := node.createSnapshotVerDelTask(m.cluster.masterAddr(), task)
			m.cluster.addLcNodeTasks(ctx, []*proto.AdminTask{adminTask})
			span.Debugf("add snapshot version del task(%v) to lcnode(%v)", *task, nodeAddr)
		}
	}
}

func (m *snapshotDelManager) notifyIdleLcNode(ctx context.Context) {
	m.lcSnapshotTaskStatus.RLock()
	defer m.lcSnapshotTaskStatus.RUnlock()
	span := proto.SpanFromContext(ctx)
	if len(m.lcSnapshotTaskStatus.VerInfos) > 0 {
		select {
		case m.idleNodeCh <- struct{}{}:
			span.Debug("action[handleLcNodeHeartbeatResp], snapshotDelManager scan routine notified!")
		default:
			span.Debug("action[handleLcNodeHeartbeatResp], snapshotDelManager skipping notify!")
		}
	}
}

//----------------------------------------------

type lcSnapshotVerStatus struct {
	sync.RWMutex
	VerInfos    map[string]*proto.SnapshotVerDelTask
	TaskResults map[string]*proto.SnapshotVerDelTaskResponse
}

func newLcSnapshotVerStatus() *lcSnapshotVerStatus {
	return &lcSnapshotVerStatus{
		VerInfos:    make(map[string]*proto.SnapshotVerDelTask),
		TaskResults: make(map[string]*proto.SnapshotVerDelTaskResponse),
	}
}

func (vs *lcSnapshotVerStatus) GetOneTask(ctx context.Context) (task *proto.SnapshotVerDelTask) {
	vs.Lock()
	defer vs.Unlock()
	if len(vs.VerInfos) == 0 {
		return
	}

	for _, i := range vs.VerInfos {
		task = i
		break
	}
	if task == nil {
		return
	}

	delete(vs.VerInfos, task.Id)
	t := time.Now()
	vs.TaskResults[task.Id] = &proto.SnapshotVerDelTaskResponse{
		ID:         task.Id,
		UpdateTime: &t,
	}
	proto.SpanFromContext(ctx).Debugf("GetOneTask(%v) and add TaskResults", task)
	return
}

func (vs *lcSnapshotVerStatus) RedoTask(task *proto.SnapshotVerDelTask) {
	vs.Lock()
	defer vs.Unlock()
	if task == nil {
		return
	}

	vs.VerInfos[task.Id] = task
}

func (vs *lcSnapshotVerStatus) AddVerInfo(ctx context.Context, task *proto.SnapshotVerDelTask) {
	vs.Lock()
	defer vs.Unlock()
	if len(vs.VerInfos) > 10000 {
		return
	}

	span := proto.SpanFromContext(ctx)
	if _, ok := vs.TaskResults[task.Id]; ok {
		span.Debugf("VerInfo: %v is in TaskResults, already in processing", task)
		return
	}
	vs.VerInfos[task.Id] = task
	span.Debugf("AddVerInfo task: %v, now num: %v", task, len(vs.VerInfos))
}

func (vs *lcSnapshotVerStatus) ResetVerInfos(ctx context.Context) {
	vs.Lock()
	defer vs.Unlock()
	proto.SpanFromContext(ctx).Debugf("ResetVerInfos remove num %v", len(vs.VerInfos))
	vs.VerInfos = make(map[string]*proto.SnapshotVerDelTask)
}

func (vs *lcSnapshotVerStatus) AddResult(resp *proto.SnapshotVerDelTaskResponse) {
	vs.Lock()
	defer vs.Unlock()
	vs.TaskResults[resp.ID] = resp
}

func (vs *lcSnapshotVerStatus) DeleteOldResult(ctx context.Context) {
	vs.Lock()
	defer vs.Unlock()
	span := proto.SpanFromContext(ctx)
	for k, v := range vs.TaskResults {
		// delete result that already done
		if v.Done && time.Now().After(v.EndTime.Add(time.Minute*10)) {
			delete(vs.TaskResults, k)
			span.Debugf("delete result already done: %v", v)
		}
		// delete result that not done but no updating
		if !v.Done && time.Now().After(v.UpdateTime.Add(time.Minute*10)) {
			delete(vs.TaskResults, k)
			span.Warnf("delete result that not done but no updating: %v", v)
		}
	}
}
