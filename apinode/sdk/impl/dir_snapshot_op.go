package impl

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"github.com/cubefs/cubefs/apinode/sdk"
	"github.com/cubefs/cubefs/blobstore/common/trace"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/google/uuid"
	"io"
	"math"
	"os"
	"path"
	"strings"
	"syscall"
)

type dirSnapshotOp struct {
	v       *volume
	mw      *snapMetaOpImp
	ec      DataOp
	rootIno uint64
}

var (
	newExtentClientVer = newDataVerOp
)

type dataOpVerImp struct {
	mw *snapMetaOpImp
	DataOp
}

func (d *dataOpVerImp) OpenStream(inode uint64) error {
	ver := d.mw.sm.GetVerInfo()
	if ver == nil {
		return d.OpenStream(inode)
	}
	return d.OpenStreamVer(inode, ver.DelVel)
}

func newDataVerOp(d DataOp, mw *snapMetaOpImp) DataOp {
	dop := &dataOpVerImp{
		mw:     mw,
		DataOp: d,
	}
	return dop
}

func (d *dirSnapshotOp) CreateDirSnapshot(ctx context.Context, ver, subPath string) error {
	span := trace.SpanFromContextSafe(ctx)

	if d.mw.conflict(ctx, subPath) {
		span.Warnf("snapshot path is conflict with before, subPath %s", subPath)
		return sdk.ErrWriteSnapshot
	}

	dirIno, _, err := d.mw.lookupSubDirVer(d.rootIno, subPath)
	if err != nil {
		span.Warnf("find snapshot path failed, rootIno %d, subPath %s, err %s",
			d.rootIno, subPath, err.Error())
		return syscallToErr(err)
	}

	vId, err := d.v.allocVerId()
	if err != nil {
		span.Warnf("alloc version from master failed, err %s", err.Error())
		return syscallToErr(err)
	}

	ifo := &proto.CreateDirSnapShotInfo{
		SnapshotDir:   subPath,
		OutVer:        ver,
		SnapshotInode: dirIno,
		RootInode:     d.rootIno,
		Ver:           vId,
	}

	err = d.mw.sm.CreateDirSnapshot(ifo)
	if err != nil {
		span.Warnf("create dir snapshot failed, req %v, err %s", ifo, err.Error())
		return syscallToErr(err)
	}

	span.Debugf("create dir snapshot success, ifo %v", ifo)
	return nil
}

func (d *dirSnapshotOp) DeleteDirSnapshot(ctx context.Context, ver, filePath string) error {
	span := trace.SpanFromContext(ctx)
	span.Info("start delete dir snapshot, ver %s, path %s, rootIno %s", ver, filePath, d.rootIno)

	dirIno, _, err := d.mw.lookupSubDirVer(d.rootIno, filePath)
	if err != nil {
		span.Warnf("lookup sub dir failed, ino %d, path %s, err %s", d.rootIno, filePath, err.Error())
		return syscallToErr(err)
	}

	ok, verInfo := d.mw.versionExist(dirIno, ver)
	if !ok {
		span.Warnf("target version is already not exist, dirIno %d, ver %s, path %s", dirIno, ver, err.Error())
		return sdk.ErrNotFound
	}

	ifo := &proto.DirVerItem{
		Ver:        verInfo.Ver,
		RootIno:    dirIno,
		DirSnapIno: dirIno,
	}
	err = d.mw.sm.DeleteDirSnapshot(ifo)
	if err != nil {
		span.Errorf("delete dir snapshot failed, ifo %v, err %s", ifo, err.Error())
		return syscallToErr(err)
	}

	span.Info("delete dir snapshot success, ver %s, path %s, rootIno %d", ver, filePath, d.rootIno)
	return nil
}

func (d *dirSnapshotOp) Lookup(ctx context.Context, parentIno uint64, name string) (*sdk.DirInfo, error) {
	span := trace.SpanFromContextSafe(ctx)

	den, err := d.mw.LookupEx(ctx, parentIno, name)
	if err != nil {
		span.Errorf("look up file failed, parIno %d, name %s, err %s", parentIno, name, err.Error())
		return nil, syscallToErr(err)
	}

	return den, nil
}

func (d *dirSnapshotOp) GetInode(ctx context.Context, ino uint64) (*proto.InodeInfo, error) {
	span := trace.SpanFromContextSafe(ctx)

	info, err := d.mw.InodeGet(ino)
	if err != nil {
		span.Errorf("get inode info failed, ino %d, err %s", ino, err.Error())
		return nil, syscallToErr(err)
	}

	return info, nil
}

func (d *dirSnapshotOp) BatchGetInodes(ctx context.Context, inos []uint64) ([]*proto.InodeInfo, error) {
	span := trace.SpanFromContextSafe(ctx)

	infos, err := d.mw.sm.BatchInodeGetWith(inos)
	if err != nil {
		span.Errorf("batchGet Inodes failed, lenInodes: %d, err %s", len(inos), err.Error())
		return nil, syscallToErr(err)
	}

	return infos, nil
}

func (d *dirSnapshotOp) Readdir(ctx context.Context, parIno uint64, marker string, count uint32) ([]sdk.DirInfo, error) {
	span := trace.SpanFromContextSafe(ctx)

	if count > 10000 {
		span.Errorf("count can't be over 10000, now %d", count)
		return nil, sdk.ErrBadRequest
	}

	if marker != "" {
		count++
	}

	dirs, err := d.mw.ReadDirLimit(parIno, marker, uint64(count))
	if err != nil {
		span.Errorf("readdir failed, parentIno: %d, marker %s, count %s, err %s", parIno, marker, count, err.Error())
		return nil, syscallToErr(err)
	}

	cnt := len(dirs)
	if cnt == 0 {
		return []sdk.DirInfo{}, nil
	}

	if marker == "" {
		return dirs[:cnt], nil
	}

	if dirs[0].Name == marker && cnt == 1 {
		return []sdk.DirInfo{}, nil
	}

	if dirs[0].Name == marker {
		return dirs[1:], nil
	}

	// all dirs bigger than marker
	if cnt == int(count) {
		return dirs[:cnt-1], nil
	}
	return dirs, nil
}

func (d *dirSnapshotOp) Rename(ctx context.Context, src, dst string) error {
	span := trace.SpanFromContextSafe(ctx)
	err := d.mw.Rename(ctx, src, dst)
	if err != nil {
		span.Errorf("rename file failed, src %v, dst %v, err %s", src, dst, err.Error())
		return syscallToErr(err)
	}
	return nil
}

func (d *dirSnapshotOp) ReadDirAll(ctx context.Context, ino uint64) ([]sdk.DirInfo, error) {
	span := trace.SpanFromContextSafe(ctx)
	marker := ""
	count := 1000
	total := make([]sdk.DirInfo, 0)

	for {
		dirs, err := d.Readdir(ctx, ino, marker, uint32(count))
		if err != nil {
			span.Errorf("readdir failed, ino %d, marker %s, count %s", ino, marker, count)
			return nil, syscallToErr(err)
		}

		total = append(total, dirs...)
		if len(dirs) < count {
			return total, nil
		}
		marker = dirs[len(dirs)-1].Name
	}
}

func (d *dirSnapshotOp) getStatByIno(ctx context.Context, ino uint64) (info *sdk.StatFs, err error) {
	span := trace.SpanFromContextSafe(ctx)
	info = new(sdk.StatFs)
	entArr, err := d.ReadDirAll(ctx, ino)
	if err != nil {
		span.Errorf("readirAll failed, ino %d, err %s", ino, err.Error())
		return nil, syscallToErr(err)
	}

	if len(entArr) == 0 {
		return &sdk.StatFs{}, nil
	}

	var files, dirs int
	inoArr := make([]uint64, 0, len(entArr))
	for _, e := range entArr {
		if proto.IsDir(e.Type) {
			subStat, err1 := d.getStatByIno(ctx, e.Inode)
			if err1 != nil {
				return nil, syscallToErr(err1)
			}
			info.Add(subStat)
			dirs++
			continue
		}

		files++
		if proto.IsRegular(e.Type) {
			inoArr = append(inoArr, e.Inode)
		}
	}
	info.Files = files

	infos, err := d.BatchGetInodes(ctx, inoArr)
	if err != nil {
		span.Errorf("batch getInodes failed, err %s", err.Error())
		return nil, syscallToErr(err)
	}

	for _, e := range infos {
		info.Size += int(e.Size)
	}

	return info, nil
}

func (d *dirSnapshotOp) StatFs(ctx context.Context, ino uint64) (*sdk.StatFs, error) {
	return d.getStatByIno(ctx, ino)
}

func (d *dirSnapshotOp) SetAttr(ctx context.Context, req *sdk.SetAttrReq) error {
	span := trace.SpanFromContextSafe(ctx)

	err := d.mw.Setattr(req.Ino, req.Flag, req.Mode, req.Uid, req.Gid, int64(req.Atime), int64(req.Mtime))
	if err != nil {
		span.Errorf("setAttr failed, ino %d, flag %d")
		return syscallToErr(err)
	}

	return nil
}

func (d *dirSnapshotOp) SetXAttr(ctx context.Context, ino uint64, key string, val string) error {
	span := trace.SpanFromContextSafe(ctx)

	err := d.mw.XAttrSet(ino, []byte(key), []byte(val))
	if err != nil {
		span.Errorf("xSetAttr failed, ino %d, key %s, err %s", ino, key, err.Error())
		return syscallToErr(err)
	}

	return nil
}

func (d *dirSnapshotOp) BatchSetXAttr(ctx context.Context, ino uint64, attrs map[string]string) error {
	span := trace.SpanFromContextSafe(ctx)

	err := d.mw.BatchSetXAttr(ino, attrs)
	if err != nil {
		span.Errorf("batch setXAttr failed, ino %d, err %s", ino, err.Error())
		return syscallToErr(err)
	}

	return nil
}

func (d *dirSnapshotOp) GetXAttr(ctx context.Context, ino uint64, key string) (string, error) {
	span := trace.SpanFromContextSafe(ctx)

	val, err := d.mw.XAttrGet_ll(ino, key)
	if err != nil {
		span.Errorf("XAttrGet failed, ino %d, key %s, err %s", ino, key, err.Error())
		return "", syscallToErr(err)
	}

	return val.XAttrs[key], nil
}

func (d *dirSnapshotOp) ListXAttr(ctx context.Context, ino uint64) ([]string, error) {
	span := trace.SpanFromContextSafe(ctx)

	val, err := d.mw.XAttrsList_ll(ino)
	if err != nil {
		span.Errorf("ListXAttr failed, ino %d, err %s", ino, err.Error())
		return nil, syscallToErr(err)
	}

	return val, nil
}

func (d *dirSnapshotOp) GetXAttrMap(ctx context.Context, ino uint64) (map[string]string, error) {
	span := trace.SpanFromContextSafe(ctx)

	val, err := d.mw.XAttrGetAll(ino)
	if err != nil {
		span.Errorf("XAttrGetAll failed, ino %d, err %s", ino, err.Error())
		return nil, syscallToErr(err)
	}

	return val.XAttrs, nil
}

func (d *dirSnapshotOp) DeleteXAttr(ctx context.Context, ino uint64, key string) error {
	span := trace.SpanFromContextSafe(ctx)

	err := d.mw.XAttrDel_ll(ino, key)
	if err != nil {
		span.Errorf("DeleteXAttr failed, ino %d, key %s, err %s", ino, key, err.Error())
		return syscallToErr(err)
	}

	return nil
}

func (d *dirSnapshotOp) BatchDeleteXAttr(ctx context.Context, ino uint64, keys []string) error {
	span := trace.SpanFromContextSafe(ctx)
	err := d.mw.XBatchDelAttr_ll(ino, keys)
	if err != nil {
		span.Errorf("batchDelXAttr failed, ino %d, err %s", ctx, err.Error())
		return syscallToErr(err)
	}

	return nil
}

const (
	defaultDirMod   = os.ModeDir | 0o755
	defaultFileMode = 0o644
)

func (d *dirSnapshotOp) Mkdir(ctx context.Context, parIno uint64, name string) (*sdk.InodeInfo, error) {
	span := trace.SpanFromContextSafe(ctx)

	info, err := d.mw.CreateFileEx(ctx, parIno, name, uint32(defaultDirMod))
	if err != nil {
		span.Errorf("create dir failed, parIno %d, name %s, err %s", parIno, name, err.Error())
		return nil, syscallToErr(err)
	}

	return info, err
}

func (d *dirSnapshotOp) CreateFile(ctx context.Context, parentIno uint64, name string) (*sdk.InodeInfo, error) {
	ifo, err := d.mw.CreateFileEx(ctx, parentIno, name, uint32(defaultFileMode))
	if err != nil {
		return nil, syscallToErr(err)
	}
	return ifo, nil
}

func (d *dirSnapshotOp) Delete(ctx context.Context, parIno uint64, name string, isDir bool) error {
	span := trace.SpanFromContextSafe(ctx)

	ifo, err := d.mw.Delete(parIno, name, isDir)
	if err != nil {
		span.Errorf("delete file failed, ino %d, name %s, dir %v, err %s", parIno, name, isDir, err.Error())
		return syscallToErr(err)
	}

	if isDir || ifo == nil {
		return nil
	}

	err = d.mw.Evict(ifo.Inode)
	if err != nil {
		span.Errorf("evict file failed, ino %d, name %s, dir %v, err %s", parIno, name, isDir, err.Error())
		return syscallToErr(err)
	}

	return nil
}

func (d *dirSnapshotOp) UploadFile(ctx context.Context, req *sdk.UploadFileReq) (*sdk.InodeInfo, error) {
	span := trace.SpanFromContextSafe(ctx)
	var oldIno uint64
	if req.OldFileId != 0 {
		//oldIno, mode, err := v.mw.Lookup_ll(req.ParIno, req.Name)
		den, err := d.mw.LookupEx(ctx, req.ParIno, req.Name)
		if err != nil && err != syscall.ENOENT {
			span.Errorf("lookup file failed, ino %d, name %s, err %s", req.ParIno, req.Name, err.Error())
			return nil, syscallToErr(err)
		}

		if den == nil || den.FileId != req.OldFileId || proto.IsDir(den.Type) {
			span.Errorf("target file already exist but conflict, den %v, reqOld %d",
				den, req.OldFileId)
			return nil, sdk.ErrConflict
		}
		oldIno = den.Inode
	}

	tmpInoInfo, err := d.mw.CreateInode(defaultFileMode)
	if err != nil {
		span.Errorf("create inode failed, err %s", err.Error())
		return nil, syscallToErr(err)
	}

	tmpIno := tmpInoInfo.Inode

	defer func() {
		// remove inode once error not nil
		if err != nil {
			_, err1 := d.mw.InodeUnlink(tmpIno)
			if err1 != nil {
				span.Errorf("unlink inode failed, ino %d, err %s", tmpIno, err1.Error())
			}

			err1 = d.mw.Evict(tmpIno)
			if err1 != nil {
				span.Errorf("evict inode failed, ino %d, err %s", tmpIno, err1.Error())
			}
		}
	}()

	err = d.ec.OpenStream(tmpIno)
	if err != nil {
		span.Errorf("open stream failed, ino %d, err %s", tmpIno, err.Error())
		return nil, syscallToErr(err)
	}

	defer func() {
		err1 := d.ec.CloseStream(tmpIno)
		if err1 != nil {
			span.Warnf("close stream failed, ino %d, err %s", tmpIno, err1.Error())
		}
	}()

	_, err = d.writeAt(ctx, tmpIno, 0, -1, req.Body)
	if err != nil {
		span.Errorf("writeAt file failed, ino %s, err %s", tmpIno, err.Error())
		return nil, err
	}

	if err = d.ec.Flush(tmpIno); err != nil {
		span.Errorf("flush file failed, ino %d, err %s", tmpIno, err.Error())
		return nil, syscallToErr(err)
	}

	var finalIno *proto.InodeInfo
	if finalIno, err = d.mw.InodeGet(tmpIno); err != nil {
		span.Errorf("get ino info failed, ino %d, err %s", tmpIno, err.Error())
		return nil, syscallToErr(err)
	}

	if cb := req.Callback; cb != nil {
		if err = cb(); err != nil {
			span.Errorf("callback, ino %d, err %s", tmpIno, err.Error())
			return nil, syscallToErr(err)
		}
	}

	if len(req.Extend) != 0 {
		err = d.mw.BatchSetXAttr(tmpIno, req.Extend)
		if err != nil {
			span.Errorf("setXAttr failed, ino %d, err %s", tmpIno, err.Error())
			return nil, syscallToErr(err)
		}
	}

	dirReq := &sdk.CreateDentryReq{
		ParentId: req.ParIno,
		Name:     req.Name,
		Inode:    tmpIno,
		OldIno:   oldIno,
		Mode:     defaultFileMode,
	}

	fileId, err := d.mw.CreateDentryEx(ctx, dirReq)
	if err != nil {
		span.Errorf("dentryCreateEx failed, parent %d, name %s, ino %d", req.ParIno, req.Name, req.OldFileId)
		return nil, syscallToErr(err)
	}

	return sdk.NewInode(finalIno, fileId), nil
	//return finalIno, nil
}

func (d *dirSnapshotOp) writeAt(ctx context.Context, ino uint64, off, size int, body io.Reader) (s int, err error) {
	span := trace.SpanFromContextSafe(ctx)

	if size < 0 {
		size = math.MaxInt64
	}

	total := 0
	wn := 0

	buf := make([]byte, util.BlockSize)
	for {
		n, err := body.Read(buf)
		if err != nil && err != io.EOF {
			span.Errorf("read file from body failed, err %s", err.Error())
			return 0, sdk.ErrBadRequest
		}

		if n > 0 {
			wn, err = d.ec.Write(ino, off, buf[:n], 0)
			if err != nil {
				span.Errorf("write file failed, ino %d, off %d, err %s", ino, off, err.Error())
				return 0, syscallToErr(err)
			}
			off += wn
		}

		total += n
		if total >= size {
			return total, nil
		}

		if err == io.EOF {
			return total, nil
		}
	}
}

func (d *dirSnapshotOp) WriteFile(ctx context.Context, ino, off, size uint64, body io.Reader) error {
	span := trace.SpanFromContextSafe(ctx)

	if err := d.ec.OpenStream(ino); err != nil {
		span.Errorf("open stream failed, ino %d, off %s, err %s", ino, off, err.Error())
		return syscallToErr(err)
	}

	defer func() {
		closeErr := d.ec.CloseStream(ino)
		if closeErr != nil {
			span.Errorf("close stream failed, ino %s, err %s", ino, closeErr.Error())
		}
	}()

	_, err := d.writeAt(ctx, ino, int(off), int(size), body)
	return err
}

func (d *dirSnapshotOp) ReadFile(ctx context.Context, ino, off uint64, data []byte) (n int, err error) {
	span := trace.SpanFromContextSafe(ctx)

	if err = d.ec.OpenStream(ino); err != nil {
		span.Errorf("open stream failed, ino %d, off %s, err %s", ino, off, err.Error())
		return 0, syscallToErr(err)
	}

	defer func() {
		closeErr := d.ec.CloseStream(ino)
		if closeErr != nil {
			span.Errorf("close stream failed, ino %s, err %s", ino, closeErr.Error())
		}
	}()

	n, err = d.ec.Read(ino, data, int(off), len(data))
	if err != nil && err != io.EOF {
		span.Errorf("read file failed, ino %d, off %d, err %s", ino, off, err.Error())
		return 0, syscallToErr(err)
	}

	return n, nil
}

func (d *dirSnapshotOp) InitMultiPart(ctx context.Context, filepath string, oldFileId uint64, extend map[string]string) (string, error) {
	if !startWithSlash(filepath) {
		return "", sdk.ErrBadRequest
	}

	span := trace.SpanFromContextSafe(ctx)

	dir, name := path.Split(filepath)
	if name == "" {
		span.Warnf("path is not illegal, path %s", filepath)
		return "", sdk.ErrBadRequest
	}

	// try check whether fileId of path is equal to oldFileId.
	if oldFileId != 0 {
		dirIno, err := d.mw.LookupPath(dir)
		if err != nil {
			span.Warnf("look up parent inode failed, dir %s, err %s", dir, err.Error())
			return "", syscallToErr(err)
		}

		ifo, err := d.mw.LookupEx(ctx, dirIno, name)
		if err != nil {
			span.Warnf("find target inode failed, path %s, dirIno %d, name %s, err %s",
				filepath, dirIno, name, err.Error())
			return "", syscallToErr(err)
		}

		if ifo.FileId != oldFileId {
			span.Errorf("init part, inode %d is conflict with exist inode %v", oldFileId, ifo)
			return "", sdk.ErrConflict
		}
	}

	uploadId, err := d.mw.InitMultipart_ll(filepath, extend)
	if err != nil {
		span.Errorf("init multipart failed, path %s, err %s", filepath, err.Error())
		return "", syscallToErr(err)
	}

	return uploadId, nil
}

func (d *dirSnapshotOp) UploadMultiPart(ctx context.Context, filepath, uploadId string, partNum uint16, read io.Reader) (part *sdk.Part, err error) {
	span := trace.SpanFromContextSafe(ctx)
	if !startWithSlash(filepath) {
		span.Warnf("input file path is not illegal, path %s", filepath)
		err = sdk.ErrBadRequest
		return
	}

	tmpInfo, err := d.mw.CreateInode(defaultFileMode)
	if err != nil {
		span.Errorf("create ino failed", err.Error())
		return nil, syscallToErr(err)
	}

	tmpIno := tmpInfo.Inode
	defer func() {
		if err != nil {
			_, err1 := d.mw.InodeUnlink(tmpIno)
			if err1 != nil {
				span.Errorf("unlink ino failed, ino %d, err %s", tmpIno, err1.Error())
			}

			err1 = d.mw.Evict(tmpIno)
			if err1 != nil {
				span.Errorf("evict ino failed, ino %d, err %s", tmpIno, err1.Error())
			}
		}
	}()

	if err = d.ec.OpenStream(tmpIno); err != nil {
		span.Errorf("openStream failed, ino %d, err %s", tmpIno, err.Error())
		return nil, syscallToErr(err)
	}

	defer func() {
		if closeErr := d.ec.CloseStream(tmpIno); closeErr != nil {
			span.Errorf("closeStream failed, ino %d, err %s", tmpIno, err.Error())
		}
	}()

	h := md5.New()
	tee := io.TeeReader(read, h)

	size, err := d.writeAt(ctx, tmpIno, 0, -1, tee)
	if err != nil {
		span.Errorf("execute writeAt failed, ino %d, err %s", tmpIno, err.Error())
		return nil, err
	}

	md5Val := hex.EncodeToString(h.Sum(nil))

	part = &sdk.Part{
		Size: uint64(size),
		ID:   partNum,
		MD5:  md5Val,
	}

	if err = d.ec.Flush(tmpIno); err != nil {
		span.Errorf("execute flush failed, ino %d, err %s", tmpIno, err.Error())
		err = syscallToErr(err)
		return
	}

	_, _, err = d.mw.AddMultipartPart_ll(filepath, uploadId, partNum, uint64(size), md5Val, tmpInfo)
	if err != nil {
		span.Errorf("add multi part failed, path %s, uploadId %s, num %d, ino %d, err %s",
			filepath, uploadId, partNum, tmpIno, err.Error())
		err = syscallToErr(err)
		return
	}

	return
}

func (d *dirSnapshotOp) ListMultiPart(ctx context.Context, filepath, uploadId string, count, marker uint64) (parts []*sdk.Part, next uint64, isTruncated bool, err error) {
	if !startWithSlash(filepath) {
		err = sdk.ErrBadRequest
		return
	}

	span := trace.SpanFromContextSafe(ctx)

	info, err := d.mw.GetMultipart_ll(filepath, uploadId)
	if err != nil {
		span.Errorf("get multipart failed, path %s, id %s, err %s", filepath, uploadId, err.Error())
		err = syscallToErr(err)
		return
	}

	sessParts := info.Parts
	total := len(sessParts)

	next = marker + count
	isTruncated = true

	if uint64(total)-marker < count {
		count = uint64(total) - marker
		next = 0
		isTruncated = false
	}

	parts = sessParts[marker : marker+count]

	return parts, next, isTruncated, nil
}

func startWithSlash(filepath string) bool {
	return strings.HasPrefix(filepath, "/")
}

func (d *dirSnapshotOp) AbortMultiPart(ctx context.Context, filepath, uploadId string) error {
	if !startWithSlash(filepath) {
		return sdk.ErrBadRequest
	}

	span := trace.SpanFromContextSafe(ctx)

	multipartInfo, err := d.mw.GetMultipart_ll(filepath, uploadId)
	if err != nil {
		span.Errorf("get multipart failed, path %s, id %s, err %s", filepath, uploadId, err.Error())
		return syscallToErr(err)
	}

	for _, part := range multipartInfo.Parts {
		if _, err = d.mw.InodeUnlink(part.Inode); err != nil {
			span.Errorf("execute inode unlink failed, ino %d, err %s", part.Inode, err.Error())
		}

		err = d.mw.Evict(part.Inode)
		if err != nil {
			span.Errorf("execute inode evict failed, ino %d, err %s", part.Inode, err.Error())
		}
	}

	if err = d.mw.RemoveMultipart_ll(filepath, uploadId); err != nil {
		span.Errorf("remove multipart failed, filepath %s, uploadId %s, err %s", filepath, uploadId, err.Error())
		return syscallToErr(err)
	}

	return nil
}

func (d *dirSnapshotOp) CompleteMultiPart(ctx context.Context, filepath, uploadId string, oldFileId uint64, partsArg []sdk.Part) (ifo *sdk.InodeInfo, err error) {
	if !startWithSlash(filepath) {
		return nil, sdk.ErrBadRequest
	}

	span := trace.SpanFromContextSafe(ctx)

	for idx, part := range partsArg {
		if part.ID != uint16(idx+1) {
			return nil, sdk.ErrInvalidPartOrder
		}
	}

	info, err := d.mw.GetMultipart_ll(filepath, uploadId)
	if err != nil {
		span.Errorf("get multipart info failed, path %s, uploadId %s, err %s", filepath, uploadId, err.Error())
		return nil, syscallToErr(err)
	}

	if len(partsArg) != len(info.Parts) {
		span.Errorf("request part is not valid, path %s, uploadId %s", filepath, uploadId)
		return nil, sdk.ErrInvalidPart
	}

	partArr := make([]*sdk.Part, 0, len(partsArg))
	for idx, part := range info.Parts {
		tmpPart := partsArg[idx]
		if tmpPart.MD5 != part.MD5 {
			span.Errorf("request part md5 is invalid, path %s, uploadId %s, num %d, md5 %s", filepath, uploadId, tmpPart.ID, tmpPart.MD5)
			return nil, sdk.ErrInvalidPart
		}
		partArr = append(partArr, part)
	}

	completeInfo, err := d.mw.CreateInode(defaultFileMode)
	if err != nil {
		span.Errorf("inode create failed, path %s, err %s", filepath, err.Error())
		return nil, syscallToErr(err)
	}
	cIno := completeInfo.Inode

	defer func() {
		if err != nil {
			if deleteErr := d.mw.InodeDelete(cIno); deleteErr != nil {
				span.Errorf("delete ino failed, ino %d, err %s", cIno, deleteErr.Error())
			}
		}
	}()

	totalExtents := make([]proto.ExtentKey, 0)
	fileOffset := uint64(0)
	size := uint64(0)
	var eks []proto.ExtentKey

	for _, part := range partArr {
		_, _, eks, err = d.mw.sm.GetExtents(part.Inode)
		if err != nil {
			span.Errorf("get part extent failed, ino %d, err %s", part.Inode, err.Error())
			return nil, syscallToErr(err)
		}

		for _, ek := range eks {
			ek.FileOffset = fileOffset
			fileOffset += uint64(ek.Size)
			totalExtents = append(totalExtents, ek)
		}
		size += part.Size
	}

	err = d.mw.AppendExtentKeys(cIno, totalExtents)
	if err != nil {
		span.Errorf("append ino to complete ino failed, ino %d, err %s", cIno, err.Error())
		return nil, syscallToErr(err)
	}

	dir, name := path.Split(filepath)
	parIno, err := d.mkdirByPath(ctx, dir)
	if err != nil {
		span.Errorf("mkdir dir failed, dir %s, err %s", dir, err.Error())
		return nil, syscallToErr(err)
	}

	err = d.mw.RemoveMultipart_ll(filepath, uploadId)
	if err != nil {
		span.Errorf("remove multipart failed, path %s, id %s, err %s", filepath, uploadId, err.Error())
		return nil, syscallToErr(err)
	}

	for _, part := range partArr {
		err1 := d.mw.InodeDelete(part.Inode)
		if err1 != nil {
			span.Errorf("delete part ino failed, ino %d, err %s", part.Inode, err1.Error())
		}
	}

	extend := info.Extend
	attrs := make(map[string]string)
	if len(extend) > 0 {
		for key, value := range extend {
			attrs[key] = value
		}

		if err = d.mw.BatchSetXAttr(cIno, attrs); err != nil {
			span.Errorf("batch setXAttr failed, ino %d", cIno, err.Error())
			return nil, err
		}
	}

	dirReq := &sdk.CreateDentryReq{
		ParentId: parIno,
		Name:     name,
		Inode:    cIno,
		OldIno:   oldFileId,
		Mode:     defaultFileMode,
	}

	fileId, err := d.mw.CreateDentryEx(ctx, dirReq)
	if err != nil {
		span.Errorf("final create dentry failed, parIno %d, name %s, childIno %d, err %s",
			parIno, name, cIno, err.Error())
		return nil, syscallToErr(err)
	}

	var newIfo *proto.InodeInfo
	newIfo, err = d.mw.InodeGet(completeInfo.Inode)
	if err != nil {
		span.Errorf("final get inode ifo failed, ino %d, err %s", completeInfo.Inode, err.Error())
		return nil, syscallToErr(err)
	}

	return sdk.NewInode(newIfo, fileId), nil
}

func (d *dirSnapshotOp) mkdirByPath(ctx context.Context, dir string) (ino uint64, err error) {
	span := trace.SpanFromContextSafe(ctx)

	parIno := proto.RootIno
	dir = strings.TrimSpace(dir)
	var childIno uint64
	var childMod uint32
	var info *sdk.InodeInfo

	defer func() {
		ino = parIno
	}()

	dirs := strings.Split(dir, "/")
	for _, name := range dirs {
		if name == "" {
			continue
		}

		childDen, err := d.mw.LookupEx(ctx, parIno, name)
		if err != nil && err != syscall.ENOENT {
			span.Errorf("lookup file failed, ino %d, name %s, err %s", parIno, name, err.Error())
			return 0, err
		}
		childIno, childMod = childDen.Inode, childDen.Type

		if err == syscall.ENOENT {
			info, err = d.mw.CreateFileEx(ctx, parIno, name, uint32(defaultDirMod))
			if err != nil && err == syscall.EEXIST {
				existDen, e := d.mw.LookupEx(ctx, parIno, name)
				if e != nil {
					span.Errorf("lookup exist ino failed, ino %d, name %s, err %s", parIno, name, err.Error())
					return 0, e
				}

				if proto.IsDir(existDen.Type) {
					parIno, err = existDen.Inode, nil
					continue
				}
			}
			if err != nil {
				span.Errorf("create dir failed, parent ino %d, name %s, err %s", parIno, name, err.Error())
				return 0, err
			}
			childIno, childMod = info.Inode, info.Mode
		}

		if !proto.IsDir(childMod) {
			span.Errorf("target file exist but not dir, ino %d, name %v", childIno, name)
			err = syscall.EINVAL
			return 0, err
		}

		parIno = childIno
	}

	return
}

func (m *dirSnapshotOp) Info() *sdk.VolInfo {
	return m.v.Info()
}

func (m *dirSnapshotOp) NewInodeLock() sdk.InodeLockApi {
	uidByte, _ := uuid.New().MarshalBinary()
	m1 := md5.New()
	m1.Write(uidByte)
	md5Val := hex.EncodeToString(m1.Sum(nil))

	lk := &InodeLock{
		v:      m.v,
		id:     md5Val,
		status: 0,
	}
	return lk
}

// GetDirSnapshot should be invoked when every rpc request from client.
func (m *dirSnapshotOp) GetDirSnapshot(ctx context.Context, rootIno uint64) (sdk.IDirSnapshot, error) {
	return nil, sdk.ErrBadRequest
}

func syscallToErr(err error) *sdk.Error {
	if err == nil {
		return nil
	}

	if newErr, ok := err.(*sdk.Error); ok {
		return newErr
	}

	switch err {
	case syscall.EAGAIN:
		return sdk.ErrRetryAgain
	case syscall.EEXIST:
		return sdk.ErrExist
	case syscall.ENOENT:
		return sdk.ErrNotFound
	case syscall.ENOTEMPTY:
		return sdk.ErrNotEmpty
	case syscall.ENOMEM:
		return sdk.ErrFull
	case syscall.EINVAL:
		return sdk.ErrBadRequest
	case syscall.EPERM:
		return sdk.ErrForbidden
	case syscall.ENOTSUP:
		return sdk.ErrConflict
	case syscall.EBADF:
		return sdk.ErrBadFile
	default:
		return sdk.ErrInternalServerError
	}
}