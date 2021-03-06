/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"fmt"
	"log"
	"io"
	"json"
	"os"
	"path/filepath"
	"sync"

	"camli/blobref"
	"camli/lru"
	"camli/schema"
	"camli/third_party/github.com/hanwen/go-fuse/fuse"
)

var _ = fmt.Println
var _ = log.Println

type CamliFileSystem struct {
	fuse.DefaultPathFilesystem

	fetcher blobref.SeekFetcher
	root    *blobref.BlobRef

	blobToSchema *lru.Cache // ~map[blobstring]*schema.Superset
	nameToBlob   *lru.Cache // ~map[string]*blobref.BlobRef
	nameToAttr   *lru.Cache // ~map[string]*fuse.Attr
}

func NewCamliFileSystem(fetcher blobref.SeekFetcher, root *blobref.BlobRef) *CamliFileSystem {
	return &CamliFileSystem{
		fetcher:      fetcher,
		blobToSchema: lru.New(1024), // arbitrary; TODO: tunable/smarter?
		root:         root,
		nameToBlob:   lru.New(1024), // arbitrary: TODO: tunable/smarter?
		nameToAttr:   lru.New(1024), // arbitrary: TODO: tunable/smarter?
	}
}

// Where name == "" for root,
// Returns nil on failure
func (fs *CamliFileSystem) blobRefFromNameCached(name string) *blobref.BlobRef {
	if br, ok := fs.nameToBlob.Get(name); ok {
		return br.(*blobref.BlobRef)
	}
	return nil
}

// Errors returned are:
//    os.ENOENT -- blob not found
//    os.EINVAL -- not JSON or a camli schema blob

func (fs *CamliFileSystem) fetchSchemaSuperset(br *blobref.BlobRef) (*schema.Superset, os.Error) {
	blobStr := br.String()
	if ss, ok := fs.blobToSchema.Get(blobStr); ok {
		return ss.(*schema.Superset), nil
	}
	log.Printf("schema cache MISS on %q", blobStr)

	rsc, _, err := fs.fetcher.Fetch(br)
	if err != nil {
		return nil, err
	}
	defer rsc.Close()
	jd := json.NewDecoder(rsc)
	ss := new(schema.Superset)
	err = jd.Decode(ss)
	if err != nil {
		log.Printf("Error parsing %s as schema blob: %v", br, err)
		return nil, os.EINVAL
	}
	if ss.Type == "" {
		log.Printf("blob %s is JSON but lacks camliType", br)
		return nil, os.EINVAL
	}
	ss.BlobRef = br
	fs.blobToSchema.Add(blobStr, ss)
	return ss, nil
}

// Where name == "" for root,
// Returns fuse.Status == fuse.OK on success or anything else on failure.
func (fs *CamliFileSystem) blobRefFromName(name string) (retbr *blobref.BlobRef, retstatus fuse.Status) {
	if name == "" {
		return fs.root, fuse.OK
	}
	if br := fs.blobRefFromNameCached(name); br != nil {
		return br, fuse.OK
	}
	defer func() {
		log.Printf("blobRefFromName(%q) = %s, %v", name, retbr, retstatus)
	}()

	dir, fileName := filepath.Split(name)
	if len(dir) > 0 {
		dir = dir[:len(dir)-1] // remove trailing "/" or whatever
	}
	dirBlob, fuseStatus := fs.blobRefFromName(dir)
	if fuseStatus != fuse.OK {
		return nil, fuseStatus
	}

	dirss, err := fs.fetchSchemaSuperset(dirBlob)
	switch {
	case err == os.ENOENT:
		log.Printf("Failed to find directory %s", dirBlob)
		return nil, fuse.ENOENT
	case err == os.EINVAL:
		log.Printf("Failed to parse directory %s", dirBlob)
		return nil, fuse.ENOTDIR
	case err != nil:
		panic(fmt.Sprintf("Invalid fetcher error: %v", err))
	case dirss == nil:
		panic("nil dirss")
	case dirss.Type != "directory":
		log.Printf("Expected %s to be a directory; actually a %s",
			dirBlob, dirss.Type)
		return nil, fuse.ENOTDIR
	}

	if dirss.Entries == "" {
		log.Printf("Expected %s to have 'entries'", dirBlob)
		return nil, fuse.ENOTDIR
	}
	entriesBlob := blobref.Parse(dirss.Entries)
	if entriesBlob == nil {
		log.Printf("Blob %s had invalid blobref %q for its 'entries'", dirBlob, dirss.Entries)
		return nil, fuse.ENOTDIR
	}

	entss, err := fs.fetchSchemaSuperset(entriesBlob)
	switch {
	case err == os.ENOENT:
		log.Printf("Failed to find entries %s via directory %s", entriesBlob, dirBlob)
		return nil, fuse.ENOENT
	case err == os.EINVAL:
		log.Printf("Failed to parse entries %s via directory %s", entriesBlob, dirBlob)
		return nil, fuse.ENOTDIR
	case err != nil:
		panic(fmt.Sprintf("Invalid fetcher error: %v", err))
	case entss == nil:
		panic("nil entss")
	case entss.Type != "static-set":
		log.Printf("Expected %s to be a directory; actually a %s",
			dirBlob, dirss.Type)
		return nil, fuse.ENOTDIR
	}

	wg := new(sync.WaitGroup)
	foundCh := make(chan *blobref.BlobRef) // important: unbuffered
	for _, m := range entss.Members {
		wg.Add(1)
		go func(memberBlobstr string) {
			defer wg.Done()
			memberBlob := blobref.Parse(memberBlobstr)
			if memberBlob == nil {
				log.Printf("invalid blobref of %q in static set %s", memberBlobstr, entss)
				return
			}
			childss, err := fs.fetchSchemaSuperset(memberBlob)
			if err == nil && childss.HasFilename(fileName) {
				foundCh <- memberBlob
			}
		}(m)
	}
	failCh := make(chan string)
	go func() {
		wg.Wait()
		failCh <- "ENOENT"
	}()
	select {
	case found := <-foundCh:
		fs.nameToBlob.Add(name, found)
		return found, fuse.OK
	case <-failCh:
	}
	// TODO: negative cache
	return nil, fuse.ENOENT
}

func (fs *CamliFileSystem) Mount(connector *fuse.PathFileSystemConnector) fuse.Status {
	log.Printf("cammount: Mount")
	return fuse.OK
}

func (fs *CamliFileSystem) Unmount() {
	log.Printf("cammount: Unmount.")
}

func (fs *CamliFileSystem) GetAttr(name string) (*fuse.Attr, fuse.Status) {
	if attr, ok := fs.nameToAttr.Get(name); ok {
		return attr.(*fuse.Attr), fuse.OK
	}

	blobref, errStatus := fs.blobRefFromName(name)
	if errStatus != fuse.OK {
		log.Printf("cammount: GetAttr(%q, %s): %v", name, blobref, errStatus)
		return nil, errStatus
	}

	ss, err := fs.fetchSchemaSuperset(blobref)
	if err != nil {
		log.Printf("cammount: GetAttr(%q, %s): fetch schema error: %v", name, blobref, err)
		return nil, fuse.EIO
	}

	out := new(fuse.Attr)
	var fi os.FileInfo

	fi.Mode = ss.UnixMode()

	// TODO: have a mode to set permissions equal to mounting user?
	fi.Uid = ss.UnixOwnerId
	fi.Gid = ss.UnixGroupId

	// TODO: other types
	if ss.Type == "file" {
		fi.Size = int64(ss.Size)
	}

	fi.Mtime_ns = schema.NanosFromRFC3339(ss.UnixMtime)
	fi.Atime_ns = fi.Mtime_ns
	fi.Ctime_ns = fi.Mtime_ns
	if atime := schema.NanosFromRFC3339(ss.UnixAtime); atime > 0 {
		fi.Atime_ns = atime
	}
	if ctime := schema.NanosFromRFC3339(ss.UnixCtime); ctime > 0 {
		fi.Ctime_ns = ctime
	}

	fuse.CopyFileInfo(&fi, out)
	fs.nameToAttr.Add(name, out)
	return out, fuse.OK
}

func (fs *CamliFileSystem) Access(name string, mode uint32) fuse.Status {
	// TODO: this is called a lot (as are many of the operations).  See
	// if we can reply to the kernel with a cache expiration time.
	//log.Printf("cammount: Access(%q, %d)", name, mode)
	return fuse.OK
}

func (fs *CamliFileSystem) Open(name string, flags uint32) (file fuse.RawFuseFile, code fuse.Status) {
	if flags&uint32(os.O_CREATE|os.O_WRONLY|os.O_RDWR|os.O_APPEND|os.O_TRUNC) != 0 {
		log.Printf("cammount: Open(%q, %d): denying write access", name, flags)
		return nil, fuse.EACCES
	}

	fileblob, errStatus := fs.blobRefFromName(name)
	log.Printf("cammount: Open(%q, %d) => (%s, %v)", name, flags, fileblob, errStatus)
	if errStatus != fuse.OK {
		return nil, errStatus
	}
	ss, err := fs.fetchSchemaSuperset(fileblob)
	if err != nil {
		log.Printf("cammount: Open(%q): %v", name, err)
		return nil, fuse.EIO
	}
	if ss.Type != "file" {
		log.Printf("cammount: Open(%q): %s is a %q, not file", name, fileblob, ss.Type)
		return nil, fuse.EINVAL
	}

	return &CamliFile{nil, fs, fileblob, ss}, fuse.OK
}

// returns fuse.OK on success; anything else on error
func (fs *CamliFileSystem) getSchemaBlobByNameAndType(name string, expectedType string) (ss *schema.Superset, status fuse.Status) {
	br, status := fs.blobRefFromName(name)
	if status != fuse.OK {
		return nil, status
	}
	return fs.getSchemaBlobByBlobRefAndType(br, expectedType)
}

func (fs *CamliFileSystem) getSchemaBlobByBlobRefAndType(br *blobref.BlobRef, expectedType string) (ss *schema.Superset, status fuse.Status) {
	ss, err := fs.fetchSchemaSuperset(br)
	switch {
	case err == os.ENOENT:
		log.Printf("failed to find blob %s", br)
		return nil, fuse.ENOENT
	case err == os.EINVAL:
		log.Printf("failed to parse expected %q schema blob %s", expectedType, br)
		return nil, fuse.EIO
	case err != nil:
		panic(fmt.Sprintf("Invalid fetcher error: %v", err))
	case ss == nil:
		panic("nil ss")
	case ss.Type != expectedType:
		log.Printf("expected %s to be %q directory; actually a %s",
			br, expectedType, ss.Type)
		return nil, fuse.EIO
	}
	return ss, fuse.OK
}

func (fs *CamliFileSystem) OpenDir(name string) (stream chan fuse.DirEntry, code fuse.Status) {
	defer func() {
		log.Printf("cammount: OpenDir(%q) = %v", name, code)
	}()
	dirss, status := fs.getSchemaBlobByNameAndType(name, "directory")
	if status != fuse.OK {
		return nil, status
	}

	if dirss.Entries == "" {
		// TODO: can this be empty for an empty directory?
		// clarify in spec one way or another.  probably best
		// to make it required to remove special cases.
		log.Printf("Expected %s to have 'entries'", dirss.BlobRef)
		return nil, fuse.ENOTDIR
	}

	entriesBlob := blobref.Parse(dirss.Entries)
	if entriesBlob == nil {
		log.Printf("Blob %s had invalid blobref %q for its 'entries'", dirss.BlobRef, dirss.Entries)
		return nil, fuse.ENOTDIR
	}

	entss, status := fs.getSchemaBlobByBlobRefAndType(entriesBlob, "static-set")
	if status != fuse.OK {
		return nil, status
	}

	retch := make(chan fuse.DirEntry, 20)
	wg := new(sync.WaitGroup)
	for _, m := range entss.Members {
		wg.Add(1)
		go func(memberBlobstr string) {
			defer wg.Done()
			memberBlob := blobref.Parse(memberBlobstr)
			if memberBlob == nil {
				log.Printf("invalid blobref of %q in static set %s", memberBlobstr, entss)
				return
			}
			childss, err := fs.fetchSchemaSuperset(memberBlob)
			if err == nil {
				if fileName := childss.FileNameString(); fileName != "" {
					mode := childss.UnixMode()
					//log.Printf("adding to dir %s: file=%q, mode=%d", dirBlob, childss.FileName, mode)
					retch <- fuse.DirEntry{Name: childss.FileNameString(), Mode: mode}
				} else {
					log.Printf("Blob %s had no filename", childss.BlobRef)
				}
			} else {
				log.Printf("Error fetching %s: %v", memberBlobstr, err)
			}
		}(m)
	}
	go func() {
		wg.Wait()
		close(retch)
	}()
	return retch, fuse.OK
}

func (fs *CamliFileSystem) Readlink(name string) (target string, status fuse.Status) {
	defer func() {
		log.Printf("Readlink(%q) = %q, %v", name, target, status)
	}()
	ss, status := fs.getSchemaBlobByNameAndType(name, "symlink")
	if status != fuse.OK {
		return "", status
	}
	return ss.SymlinkTargetString(), fuse.OK
}

type CamliFile struct {
	*fuse.DefaultRawFuseFile

	fs   *CamliFileSystem
	blob *blobref.BlobRef
	ss   *schema.Superset
}

func (file *CamliFile) Read(ri *fuse.ReadIn, bp *fuse.BufferPool) (retbuf []byte, retst fuse.Status) {
	offset := ri.Offset
	if offset >= file.ss.Size {
		return []byte(""), fuse.OK // TODO: correct status?
	}
	size := ri.Size // size of read to do (uint32)
	endOffset := offset + uint64(size)
	if endOffset > file.ss.Size {
		size -= uint32(endOffset - file.ss.Size)
		endOffset = file.ss.Size
	}

	buf := bytes.NewBuffer(make([]byte, 0, int(size)))
	fr := file.ss.NewFileReader(file.fs.fetcher)
	fr.Skip(offset)
	lr := io.LimitReader(fr, int64(size))
	_, err := io.Copy(buf, lr) // TODO: care about n bytes copied?
	if err == nil {
		return buf.Bytes(), fuse.OK
	}
	log.Printf("cammount Read error: %v", err)
	retst = fuse.EIO
	return
}
