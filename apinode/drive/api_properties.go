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

package drive

import (
	"github.com/cubefs/cubefs/blobstore/common/rpc"
)

type GetPropertiesResult struct {
	ID         uint64            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Size       int64             `json:"size"`
	Ctime      int64             `json:"ctime"`
	Mtime      int64             `json:"mtime"`
	Atime      int64             `json:"atime"`
	Properties map[string]string `json:"properties"`
}

func (d *DriveNode) handleSetProperties(c *rpc.Context) {
	ctx, span := d.ctxSpan(c)
	uid := d.userID(c)

	args := new(ArgsSetProperties)
	if d.checkError(c, nil, c.ParseArgs(args)) {
		return
	}
	if d.checkError(c, func(err error) { span.Info(args.Path, err) }, args.Path.Clean()) {
		return
	}

	xattrs, err := d.getProperties(c)
	if d.checkError(c, func(err error) { span.Info(err) }, err) {
		return
	}
	if len(xattrs) == 0 {
		c.Respond()
		return
	}
	span.Info("to set xattrs:", xattrs)

	rootIno, vol, err := d.getRootInoAndVolume(ctx, uid)
	if d.checkError(c, func(err error) { span.Errorf("get user router uid=%s error: %v", uid, err) }, err) {
		return
	}
	dirInfo, err := d.lookup(ctx, vol, rootIno, args.Path.String())
	if d.checkError(c, func(err error) { span.Errorf("lookup path=%s error: %v", args.Path, err) }, err) {
		return
	}
	if d.checkFunc(c, func(err error) { span.Errorf("batch set xattr path=%s error: %v", args.Path, err) },
		func() error { return vol.BatchSetXAttr(ctx, dirInfo.Inode, xattrs) }) {
		return
	}
	c.Respond()
}

func (d *DriveNode) handleDelProperties(c *rpc.Context) {
	ctx, span := d.ctxSpan(c)
	uid := d.userID(c)

	args := new(ArgsDelProperties)
	if d.checkError(c, nil, c.ParseArgs(args)) {
		return
	}
	if d.checkError(c, func(err error) { span.Info(args.Path, err) }, args.Path.Clean()) {
		return
	}

	xattrs, err := d.getProperties(c)
	if d.checkError(c, func(err error) { span.Info(err) }, err) {
		return
	}
	if len(xattrs) == 0 {
		c.Respond()
		return
	}

	keys := make([]string, 0, len(xattrs))
	for k := range xattrs {
		keys = append(keys, k)
	}
	span.Info("to del xattrs:", keys)

	rootIno, vol, err := d.getRootInoAndVolume(ctx, uid)
	if d.checkError(c, func(err error) { span.Errorf("get user router uid=%s error: %v", uid, err) }, err) {
		return
	}
	dirInfo, err := d.lookup(ctx, vol, rootIno, args.Path.String())
	if d.checkError(c, func(err error) { span.Errorf("lookup path=%s error: %v", args.Path, err) }, err) {
		return
	}
	if d.checkFunc(c, func(err error) { span.Errorf("batch del xattr path=%s error: %v", args.Path, err) },
		func() error { return vol.BatchDeleteXAttr(ctx, dirInfo.Inode, keys) }) {
		return
	}
	c.Respond()
}

func (d *DriveNode) handleGetProperties(c *rpc.Context) {
	ctx, span := d.ctxSpan(c)
	uid := d.userID(c)

	args := new(ArgsGetProperties)
	if d.checkError(c, nil, c.ParseArgs(args)) {
		return
	}
	if d.checkError(c, func(err error) { span.Info(args.Path, err) }, args.Path.Clean()) {
		return
	}

	rootIno, vol, err := d.getRootInoAndVolume(ctx, uid)
	if d.checkError(c, func(err error) { span.Errorf("get user router uid=%s error: %v", uid, err) }, err) {
		return
	}
	dirInfo, err := d.lookup(ctx, vol, rootIno, args.Path.String())
	if d.checkError(c, func(err error) { span.Errorf("lookup path=%s error: %v", args.Path, err) }, err) {
		return
	}
	xattrs, err := vol.GetXAttrMap(ctx, dirInfo.Inode)
	if d.checkError(c, func(err error) { span.Errorf("get xattr path=%s error: %v", args.Path, err) }, err) {
		return
	}
	inoInfo, err := vol.GetInode(ctx, dirInfo.Inode)
	if d.checkError(c, func(err error) { span.Errorf("get inode path=%s error: %v", args.Path, err) }, err) {
		return
	}
	res := GetPropertiesResult{
		ID:         inoInfo.Inode,
		Name:       dirInfo.Name,
		Type:       "file",
		Size:       int64(inoInfo.Size),
		Ctime:      inoInfo.CreateTime.Unix(),
		Mtime:      inoInfo.ModifyTime.Unix(),
		Atime:      inoInfo.AccessTime.Unix(),
		Properties: xattrs,
	}
	if dirInfo.IsDir() {
		res.Type = "folder"
	}
	d.respData(c, res)
}
