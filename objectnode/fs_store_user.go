// Copyright 2019 The CubeFS Authors.
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

package objectnode

import (
	"context"
	"fmt"
	"hash/crc32"
	"sync"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/util/exporter"
)

const (
	updateUserStoreInterval      = time.Minute * 1
	userBlacklistCleanupInterval = time.Minute * 1
	userBlacklistTTL             = time.Second * 10
	userInfoLoaderNum            = 4
)

type UserInfoStore interface {
	LoadUser(ctx context.Context, accessKey string) (*proto.UserInfo, error)
}

type StrictUserInfoStore struct {
	mc *master.MasterClient
}

func (s *StrictUserInfoStore) LoadUser(ctx context.Context, accessKey string) (*proto.UserInfo, error) {
	span := spanWithOperation(ctx, "LoadUser")
	// if error occurred when loading user, and error is not NotExist, output an ump log
	userInfo, err := s.mc.UserAPI().GetAKInfo(ctx, accessKey)
	if err != nil && err != proto.ErrUserNotExists && err != proto.ErrAccessKeyNotExists {
		span.Errorf("get user info from master fail: accessKey(%v) err(%v)", accessKey, err)
		exporter.Warning(fmt.Sprintf("StrictUserInfoStore load user fail: accessKey(%v) err(%v)", accessKey, err))
	}
	return userInfo, err
}

type CacheUserInfoStore struct {
	mc      *master.MasterClient
	loaders [userInfoLoaderNum]*CacheUserInfoLoader
}

func (s *CacheUserInfoStore) selectLoader(accessKey string) *CacheUserInfoLoader {
	i := crc32.ChecksumIEEE([]byte(accessKey)) % userInfoLoaderNum
	return s.loaders[i]
}

func (s *CacheUserInfoStore) Close() {
	for _, loader := range s.loaders {
		loader.Close()
	}
}

func (s *CacheUserInfoStore) LoadUser(ctx context.Context, accessKey string) (*proto.UserInfo, error) {
	return s.selectLoader(accessKey).LoadUser(ctx, accessKey)
}

func NewUserInfoStore(masters []string, strict bool) UserInfoStore {
	mc := master.NewMasterClient(masters, false)
	if strict {
		return &StrictUserInfoStore{
			mc: mc,
		}
	}
	store := &CacheUserInfoStore{
		mc: mc,
	}
	for i := 0; i < userInfoLoaderNum; i++ {
		store.loaders[i] = NewUserInfoLoader(mc)
	}
	return store
}

func ReleaseUserInfoStore(store UserInfoStore) {
	if cacheStore, is := store.(*CacheUserInfoStore); is {
		cacheStore.Close()
	}
}

type CacheUserInfoLoader struct {
	mc          *master.MasterClient
	akInfoStore map[string]*proto.UserInfo // mapping: access key -> user info (*proto.UserInfo)
	akInfoMutex sync.RWMutex
	akInitMap   sync.Map // mapping: access key -> *sync.Mutex
	blacklist   sync.Map // mapping: access key -> timestamp (time.Time)
	closeCh     chan struct{}
	closeOnce   sync.Once
}

func NewUserInfoLoader(mc *master.MasterClient) *CacheUserInfoLoader {
	us := &CacheUserInfoLoader{
		mc:          mc,
		akInfoStore: make(map[string]*proto.UserInfo),
		closeCh:     make(chan struct{}, 1),
	}
	go us.scheduleUpdate()
	go us.blacklistCleanup()
	return us
}

func (us *CacheUserInfoLoader) blacklistCleanup() {
	t := time.NewTimer(userBlacklistCleanupInterval)
	for {
		select {
		case <-t.C:
		case <-us.closeCh:
			t.Stop()
			return
		}
		us.blacklist.Range(func(key, value interface{}) bool {
			ts, is := value.(time.Time)
			if !is || time.Since(ts) > userBlacklistTTL {
				us.blacklist.Delete(key)
			}
			return true
		})
		t.Reset(userBlacklistCleanupInterval)
	}
}

func (us *CacheUserInfoLoader) scheduleUpdate() {
	t := time.NewTimer(updateUserStoreInterval)
	aks := make([]string, 0)
	for {
		select {
		case <-t.C:
		case <-us.closeCh:
			t.Stop()
			return
		}

		aks = aks[:0]
		us.akInfoMutex.RLock()
		for ak := range us.akInfoStore {
			aks = append(aks, ak)
		}
		us.akInfoMutex.RUnlock()

		_, ctx := proto.SpanContext()
		span := spanWithOperation(ctx, "CacheUserInfoLoader")
		for _, ak := range aks {
			akPolicy, err := us.mc.UserAPI().GetAKInfo(ctx, ak)
			if err == proto.ErrUserNotExists || err == proto.ErrAccessKeyNotExists {
				us.akInfoMutex.Lock()
				delete(us.akInfoStore, ak)
				us.akInfoMutex.Unlock()
				us.blacklist.Store(ak, time.Now())
				span.Debugf("release user info and add to blacklist: accessKey(%v)", ak)
				continue
			}
			// if error info is not empty, it means error occurred communication with master, output an ump log
			if err != nil {
				span.Errorf("get user info from master fail: accessKey(%v) err(%v)", ak, err)
				exporter.Warning(fmt.Sprintf("CacheUserInfoLoader get user info fail when scheduling update: err(%v)", err))
				break
			}
			us.akInfoMutex.Lock()
			us.akInfoStore[ak] = akPolicy
			us.akInfoMutex.Unlock()
		}
		t.Reset(updateUserStoreInterval)
	}
}

func (us *CacheUserInfoLoader) syncUserInit(accessKey string) (releaseFunc func()) {
	value, _ := us.akInitMap.LoadOrStore(accessKey, new(sync.Mutex))
	initMu := value.(*sync.Mutex)
	initMu.Lock()

	return func() {
		initMu.Unlock()
		us.akInitMap.Delete(accessKey)
	}
}

func (us *CacheUserInfoLoader) LoadUser(ctx context.Context, accessKey string) (userInfo *proto.UserInfo, err error) {
	span := spanWithOperation(ctx, "LoadUser")
	// Check if the access key is on the blacklist.
	if val, exist := us.blacklist.Load(accessKey); exist {
		if ts, is := val.(time.Time); is {
			if time.Since(ts) <= userBlacklistTTL {
				span.Debugf("load by ak(%v) from blacklist and not expired, return ErrUserNotExists",
					accessKey)
				err = proto.ErrUserNotExists
				return
			}
		}
	}

	us.akInfoMutex.RLock()
	userInfo, exist := us.akInfoStore[accessKey]
	us.akInfoMutex.RUnlock()
	if !exist {
		release := us.syncUserInit(accessKey)
		defer release()

		us.akInfoMutex.RLock()
		userInfo, exist = us.akInfoStore[accessKey]
		if exist {
			us.akInfoMutex.RUnlock()
			return
		}
		us.akInfoMutex.RUnlock()

		userInfo, err = us.mc.UserAPI().GetAKInfo(ctx, accessKey)
		if err != nil {
			span.Debugf("get user info from master fail and add to blacklist: accessKey(%v) err(%v)",
				accessKey, err)
			if err != proto.ErrUserNotExists && err != proto.ErrAccessKeyNotExists {
				span.Errorf("get user info from master fail and add to blacklist: accessKey(%v) err(%v)",
					accessKey, err)
				// if error occurred when loading user, and error is not NotExist, output an ump log
				exporter.Warning(fmt.Sprintf("CacheUserInfoLoader load user info fail: accessKey(%v) err(%v)", accessKey, err))
			}
			us.blacklist.Store(accessKey, time.Now())
			return
		}

		us.akInfoMutex.Lock()
		us.akInfoStore[accessKey] = userInfo
		us.akInfoMutex.Unlock()

		span.Debugf("get user info success: accessKey(%v) userInfo(%+v)", accessKey, userInfo)
	}

	return
}

func (us *CacheUserInfoLoader) Close() {
	us.akInfoMutex.Lock()
	defer us.akInfoMutex.Unlock()
	us.closeOnce.Do(func() {
		close(us.closeCh)
	})
}
