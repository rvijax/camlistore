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

package schema

import (
	"fmt"
	"io"
	"json"
	"log"
	"os"

	"camli/blobref"
)

var _ = log.Printf

const closedIndex = -1
var errClosed = os.NewError("filereader is closed")

type FileReader struct {
	fetcher blobref.SeekFetcher
	ss      *Superset

	ci     int    // index into contentparts, or -1 on closed
	ccon   uint64 // bytes into current chunk already consumed
	remain int64  // bytes remaining

	cr   blobref.ReadSeekCloser // cached reader (for blobref chunks)
	crbr *blobref.BlobRef       // the blobref that cr is for

	csubfr *FileReader  // cached sub blobref reader (for subBlobRef chunks)
	ccp    *ContentPart // the content part that csubfr is cached for
}

// TODO: make this take a blobref.FetcherAt instead?
func NewFileReader(fetcher blobref.SeekFetcher, fileBlobRef *blobref.BlobRef) (*FileReader, os.Error) {
	if fileBlobRef == nil {
		return nil, os.NewError("schema/filereader: NewFileReader blobref was nil")
	}
	ss := new(Superset)
	rsc, _, err := fetcher.Fetch(fileBlobRef)
	if err != nil {
		return nil, fmt.Errorf("schema/filereader: fetching file schema blob: %v", err)
	}
	if err = json.NewDecoder(rsc).Decode(ss); err != nil {
		return nil, fmt.Errorf("schema/filereader: decoding file schema blob: %v", err)
	}
	if ss.Type != "file" {
		return nil, fmt.Errorf("schema/filereader: expected \"file\" schema blob, got %q", ss.Type)
	}
	return ss.NewFileReader(fetcher), nil
}

func (ss *Superset) NewFileReader(fetcher blobref.SeekFetcher) *FileReader {
	// TODO: return an error if ss isn't a Type "file"
	//
	return &FileReader{fetcher: fetcher, ss: ss, remain: int64(ss.Size)}
}

// FileSchema returns the reader's schema superset. Don't mutate it.
func (fr *FileReader) FileSchema() *Superset {
	return fr.ss
}

func (fr *FileReader) Close() os.Error {
	if fr.ci == closedIndex {
		return errClosed
	}
	fr.closeOpenBlobs()
	fr.ci = closedIndex
	return nil
}

func (fr *FileReader) Skip(skipBytes uint64) uint64 {
	if fr.ci == closedIndex {
		return 0
	}

	wantedSkipped := skipBytes

	for skipBytes != 0 && fr.ci < len(fr.ss.ContentParts) {
		cp := fr.ss.ContentParts[fr.ci]
		thisChunkSkippable := cp.Size - fr.ccon
		toSkip := minu64(skipBytes, thisChunkSkippable)
		fr.ccon += toSkip
		fr.remain -= int64(toSkip)
		if fr.ccon == cp.Size {
			fr.ci++
			fr.ccon = 0
		}
		skipBytes -= toSkip
	}

	return wantedSkipped - skipBytes
}

func (fr *FileReader) closeOpenBlobs() {
	if fr.cr != nil {
		fr.cr.Close()
		fr.cr = nil
		fr.crbr = nil
	}
}

func (fr *FileReader) readerFor(br *blobref.BlobRef, seekTo int64) (r io.Reader, err os.Error) {
	if fr.crbr == br {
		return fr.cr, nil
	}
	fr.closeOpenBlobs()
	var rsc blobref.ReadSeekCloser
	if br != nil {
		rsc, _, err = fr.fetcher.Fetch(br)
		if err != nil {
			return
		}

		_, serr := rsc.Seek(int64(seekTo), os.SEEK_SET)
		if serr != nil {
			return nil, fmt.Errorf("schema: FileReader.Read seek error on blob %s: %v", br, serr)
		}

	} else {
		rsc = &zeroReader{}
	}
	fr.crbr = br
	fr.cr = rsc
	return rsc, nil
}

func (fr *FileReader) subBlobRefReader(cp *ContentPart) (io.Reader, os.Error) {
	if fr.ccp == cp {
		return fr.csubfr, nil
	}
	subfr, err := NewFileReader(fr.fetcher, cp.SubBlobRef)
	if err == nil {
		subfr.Skip(cp.Offset)
		fr.csubfr = subfr
		fr.ccp = cp
	}
	return subfr, err
}

func (fr *FileReader) currentPart() (*ContentPart, os.Error) {
	for {
		if fr.ci >= len(fr.ss.ContentParts) {
			fr.closeOpenBlobs()
			if fr.remain > 0 {
				return nil, fmt.Errorf("schema: declared file schema size was larger than sum of content parts")
			}
			return nil, os.EOF
		}
		cp := fr.ss.ContentParts[fr.ci]
		thisChunkReadable := cp.Size - fr.ccon
		if thisChunkReadable == 0 {
			fr.ci++
			fr.ccon = 0
			continue
		}
		return cp, nil
	}
	panic("unreachable")
}

func (fr *FileReader) Read(p []byte) (n int, err os.Error) {
	if fr.ci == closedIndex {
		return 0, errClosed
	}

	cp, err := fr.currentPart()
	if err != nil {
		return 0, err
	}

	if cp.Size == 0 {
		return 0, fmt.Errorf("blobref content part contained illegal size 0")
	}

	br := cp.BlobRef
	sbr := cp.SubBlobRef
	if br != nil && sbr != nil {
		return 0, fmt.Errorf("content part index %d has both blobRef and subFileBlobRef", fr.ci)
	}

	var r io.Reader

	if sbr != nil {
		r, err = fr.subBlobRefReader(cp)
		if err != nil {
			return 0, fmt.Errorf("schema: FileReader.Read error fetching sub file %s: %v", sbr, err)
		}
	} else {
		seekTo := cp.Offset + fr.ccon
		r, err = fr.readerFor(br, int64(seekTo))
		if err != nil {
			return 0, fmt.Errorf("schema: FileReader.Read error fetching blob %s: %v", br, err)
		}
	}

	readSize := cp.Size - fr.ccon
	if readSize < uint64(len(p)) {
		p = p[:int(readSize)]
	}

	n, err = r.Read(p)
	fr.ccon += uint64(n)
	fr.remain -= int64(n)
	if fr.remain < 0 {
		err = fmt.Errorf("schema: file schema was invalid; content parts sum to over declared size")
	}
	return
}

func minu64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

type zeroReader struct{}

func (*zeroReader) Read(p []byte) (int, os.Error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func (*zeroReader) Close() os.Error {
	return nil
}

func (*zeroReader) Seek(offset int64, whence int) (newFilePos int64, err os.Error) {
	// Caller is ignoring our newFilePos return value.
	return 0, nil
}
