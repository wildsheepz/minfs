/*
 * MinFS - fuse driver for Object Storage (C) 2016 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package minfs

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"github.com/minio/minfs/meta"
	minio "github.com/minio/minio-go/v6"
)

// File implements both Node and Handle for the hello file.
type File struct {
	mfs *MinFS

	dir *Dir

	Path string

	Inode uint64

	Mode os.FileMode

	Size uint64
	ETag string

	Atime time.Time
	Mtime time.Time

	UID uint32
	GID uint32

	// OS X only
	Bkuptime time.Time
	Chgtime  time.Time
	Crtime   time.Time
	Flags    uint32 // see chflags(2)

	Hash []byte
}

func (f *File) store(tx *meta.Tx) error {
	b := f.bucket(tx)
	return b.Put(path.Base(f.Path), f)
}

// Attr - attr file context.
func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {
	*a = fuse.Attr{
		Inode:  f.Inode,
		Size:   f.Size,
		Atime:  f.Atime,
		Mtime:  f.Mtime,
		Ctime:  f.Chgtime,
		Crtime: f.Crtime,
		Mode:   f.Mode,
		Uid:    f.UID,
		Gid:    f.GID,
		Flags:  f.Flags,
	}

	return nil
}

// Setattr - set attribute.
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	// update cache with new attributes
	return f.mfs.db.Update(func(tx *meta.Tx) error {
		if req.Valid.Mode() {
			f.Mode = req.Mode
		}

		if req.Valid.Uid() {
			f.UID = req.Uid
		}

		if req.Valid.Gid() {
			f.GID = req.Gid
		}

		if req.Valid.Size() {
			f.Size = req.Size
		}

		if req.Valid.Atime() {
			f.Atime = req.Atime
		}

		if req.Valid.Mtime() {
			f.Mtime = req.Mtime
		}

		if req.Valid.Crtime() {
			f.Crtime = req.Crtime
		}

		if req.Valid.Chgtime() {
			f.Chgtime = req.Chgtime
		}

		if req.Valid.Bkuptime() {
			f.Bkuptime = req.Bkuptime
		}

		if req.Valid.Flags() {
			f.Flags = req.Flags
		}

		return f.store(tx)
	})
}

// RemotePath will return the full path on bucket
func (f *File) RemotePath() string {
	return path.Join(f.dir.RemotePath(), f.Path)
}

// FullPath will return the full path
func (f *File) FullPath() string {
	return path.Join(f.dir.FullPath(), f.Path)
}

// Saves a new file at cached path and fetches the object based on
// the incoming fuse request.
func (f *File) cacheSave(path string, req *fuse.OpenRequest) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	if req.Flags&fuse.OpenTruncate == fuse.OpenTruncate {
		f.Size = 0
		return nil
	}

	object, err := f.mfs.api.GetObject(f.mfs.config.bucket, f.RemotePath(), minio.GetObjectOptions{})
	if err != nil {
		if meta.IsNoSuchObject(err) {
			return fuse.ENOENT
		}
		return err
	}
	defer object.Close()

	hasher := sha256.New()
	size, err := io.Copy(file, io.TeeReader(object, hasher))
	if err != nil {
		return err
	}

	// update actual file size
	f.Size = uint64(size)

	// hash will be used when encrypting files
	_ = hasher.Sum(nil)

	// Success.
	return nil
}

// Open return a file handle of the opened file
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	if err := f.dir.mfs.wait(f.Path); err != nil {
		return nil, err
	}

	// Start a writable transaction.
	tx, err := f.mfs.db.Begin(true)
	if err != nil {
		return nil, err
	}

	defer tx.Rollback()

	cachePath, err := f.dir.mfs.NewCachePath()
	if err != nil {
		return nil, err
	}

	err = f.cacheSave(cachePath, req)
	if err != nil {
		return nil, err
	}

	fh, err := f.mfs.Acquire(f)
	if err != nil {
		return nil, err
	}

	fh.cachePath = cachePath

	fh.File, err = os.OpenFile(fh.cachePath, int(req.Flags), f.mfs.config.mode)
	if err != nil {
		return nil, err
	}

	if err = f.store(tx); err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, err
	}

	resp.Handle = fuse.HandleID(fh.handle)
	return fh, nil
}

func (f *File) bucket(tx *meta.Tx) *meta.Bucket {
	b := f.dir.bucket(tx)
	return b
}

// Getattr returns the file attributes
func (f *File) Getattr(ctx context.Context, req *fuse.GetattrRequest, resp *fuse.GetattrResponse) error {
	resp.Attr = fuse.Attr{
		Inode:  f.Inode,
		Size:   f.Size,
		Atime:  f.Atime,
		Mtime:  f.Mtime,
		Ctime:  f.Chgtime,
		Crtime: f.Crtime,
		Mode:   f.Mode,
		Uid:    f.UID,
		Gid:    f.GID,
		Flags:  f.Flags,
	}

	return nil
}

// Dirent returns the File object as a fuse.Dirent
func (f *File) Dirent() fuse.Dirent {
	return fuse.Dirent{
		Inode: f.Inode, Name: f.Path, Type: fuse.DT_File,
	}
}

func (f *File) delete(tx *meta.Tx) error {
	// purge from cache
	b := f.bucket(tx)
	return b.Delete(f.Path)
}
