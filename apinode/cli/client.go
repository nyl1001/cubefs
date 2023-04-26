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

package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/cubefs/cubefs/apinode/drive"
	"github.com/cubefs/cubefs/blobstore/common/rpc"
)

const (
	post = http.MethodPost
	put  = http.MethodPut
	get  = http.MethodGet
	del  = http.MethodDelete
)

var cli = &client{Client: rpc.NewClient(&rpc.Config{})}

type client struct {
	rpc.Client
}

func (c *client) request(method string, uri string, body io.Reader, meta ...string) error {
	return c.requestWith(method, uri, body, nil, meta...)
}

func (c *client) requestWith(method string, uri string, body io.Reader, ret interface{}, meta ...string) error {
	req, err := http.NewRequest(method, host+uri, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-cfa-service", "drive")
	req.Header.Set("x-cfa-user-id", user)
	for i := 0; i < len(meta); i += 2 {
		req.Header.Set("x-cfa-meta-"+meta[i], meta[i+1])
	}

	resp, err := c.Do(context.Background(), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return rpc.ParseData(resp, ret)
}

func (c *client) requestWithHeader(method string, uri string, body io.Reader, headers map[string]string,
	ret interface{}, meta ...string) error {
	req, err := http.NewRequest(method, host+uri, body)
	if err != nil {
		return err
	}
	req.Header.Set("x-cfa-service", "drive")
	req.Header.Set("x-cfa-user-id", user)
	for i := 0; i < len(meta); i += 2 {
		req.Header.Set("x-cfa-meta-"+meta[i], meta[i+1])
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.Do(context.Background(), req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return rpc.ParseData(resp, ret)
}

func (c *client) DriveCreate() (r drive.CreateDriveResult, err error) {
	err = c.requestWith(put, "/v1/drive", nil, &r)
	return
}

func (c *client) DriveGet() (r drive.UserRoute, err error) {
	err = c.requestWith(get, "/v1/drive", nil, &r)
	return
}

func (c *client) ConfigAdd(path string) error {
	return c.request(put, genURI("/v1/user/config", "path", path), nil)
}

func (c *client) ConfigGet() (r drive.GetUserConfigResult, err error) {
	err = c.requestWith(get, "/v1/user/config", nil, &r)
	return
}

func (c *client) ConfigDel(path string) error {
	return c.request(del, genURI("/v1/user/config", "path", path), nil)
}

func (c *client) MetaSet(path string, meta ...string) error {
	return c.request(post, genURI("/v1/meta", "path", path), nil, meta...)
}

func (c *client) MetaGet(path string) (r drive.GetPropertiesResult, err error) {
	err = c.requestWith(get, genURI("/v1/meta", "path", path), nil, &r)
	return
}

func (c *client) ListDir(path, marker, limit, filter string) (r drive.ListDirResult, err error) {
	err = c.requestWith(get, genURI("/v1/files",
		"path", path, "marker", marker, "limit", limit, "filter", filter), nil, &r)
	return
}

func (c *client) MkDir(path string, recursive bool) error {
	return c.request(post, genURI("/v1/files/mkdir", "path", path, "recursive", recursive), nil)
}

func (c *client) FileUpload(path string, fileID uint64, body io.Reader, meta ...string) (r drive.FileInfo, err error) {
	err = c.requestWith(put, genURI("/v1/files/upload", "path", path, "fileId", fileID), body, &r, meta...)
	return
}

func (c *client) FileWrite(fileID uint64, from, to int, body io.Reader) error {
	return c.requestWithHeader(put, genURI("/v1/files/content", "fileId", fileID), body,
		map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", from, to)}, nil)
}

func (c *client) FileDownload(path string, from, to int) (r io.ReadCloser, err error) {
	req, err := http.NewRequest(get, host+genURI("/v1/files/content", "path", path), nil)
	if err != nil {
		return
	}
	req.Header.Set("x-cfa-service", "drive")
	req.Header.Set("x-cfa-user-id", user)
	if from >= 0 && to >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	}

	resp, err := c.Do(context.Background(), req)
	if err != nil {
		return
	}
	if resp.StatusCode == 200 || resp.StatusCode == 206 {
		r = resp.Body
		return
	}
	err = rpc.ParseData(resp, nil)
	return
}

func (c *client) FileCopy(src, dst string, meta bool) error {
	return c.request(post, genURI("/v1/files/copy", "src", src, "dst", dst, "meta", meta), nil)
}

func (c *client) FileRename(src, dst string) error {
	return c.request(post, genURI("/v1/files/rename", "src", src, "dst", dst), nil)
}

func genURI(uri string, queries ...interface{}) string {
	if len(queries)%2 == 1 {
		queries = append(queries, "")
	}
	q := make(url.Values)
	for i := 0; i < len(queries); i += 2 {
		q.Set(fmt.Sprint(queries[i]), fmt.Sprint(queries[i+1]))
	}
	if len(q) > 0 {
		return uri + "?" + q.Encode()
	}
	return uri
}
