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
	"bufio"
	"bytes"
	"crypto"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"camli/blobref"
	"camli/blobserver"
	"camli/rollsum"
)

var _ = log.Printf

// WriteFileFromReader creates and uploads a "file" JSON schema
// composed of chunks of r, also uploading the chunks.  The returned
// BlobRef is of the JSON file schema blob.
func WriteFileFromReader(bs blobserver.Storage, filename string, r io.Reader) (*blobref.BlobRef, os.Error) {
	// Naive for now.  Just in 1MB chunks.
	// TODO: rolling hash and hash trees.

	parts, size := []ContentPart{}, int64(0)

	buf := new(bytes.Buffer)
	for {
		buf.Reset()

		n, err := io.Copy(buf, io.LimitReader(r, 1<<20))
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}

		hash := crypto.SHA1.New()
		io.Copy(hash, bytes.NewBuffer(buf.Bytes()))
		br := blobref.FromHash("sha1", hash)
		hasBlob, err := serverHasBlob(bs, br)
		if err != nil {
			return nil, err
		}
		if !hasBlob {
			sb, err := bs.ReceiveBlob(br, buf)
			if err != nil {
				return nil, err
			}
			if expect := (blobref.SizedBlobRef{br, n}); !expect.Equal(sb) {
				return nil, fmt.Errorf("schema/filewriter: wrote %s bytes, got %s ack'd", expect, sb)
			}
		}

		size += n
		parts = append(parts, ContentPart{
			BlobRef:       br,
			Size:          uint64(n),
			Offset:        0, // into BlobRef to read from (not of dest)
		})
	}

	m := NewCommonFilenameMap(filename)
	err := PopulateRegularFileMap(m, size, parts)
	if err != nil {
		return nil, err
	}

	json, err := MapToCamliJson(m)
	if err != nil {
		return nil, err
	}
	br := blobref.Sha1FromString(json)
	sb, err := bs.ReceiveBlob(br, strings.NewReader(json))
	if err != nil {
		return nil, err
	}
	if expect := (blobref.SizedBlobRef{br, int64(len(json))}); !expect.Equal(sb) {
		return nil, fmt.Errorf("schema/filewriter: wrote %s bytes, got %s ack'd", expect, sb)
	}

	return br, nil
}

func serverHasBlob(bs blobserver.Storage, br *blobref.BlobRef) (have bool, err os.Error) {
	ch := make(chan blobref.SizedBlobRef, 1)
	go func() {
		err = bs.Stat(ch, []*blobref.BlobRef{br}, 0)
		close(ch)
	}()
	for _ = range ch {
		have = true
	}
	return
}

type span struct {
	from, to int64
	bits     int
	br       *blobref.BlobRef
	children []span
}

func (s *span) size() int64 {
	size := s.to - s.from
	for _, cs := range s.children {
		size += cs.size()
	}
	return size
}

// WriteFileFromReaderRolling creates and uploads a "file" JSON schema
// composed of chunks of r, also uploading the chunks.  The returned
// BlobRef is of the JSON file schema blob.
func WriteFileFromReaderRolling(bs blobserver.Storage, filename string, r io.Reader) (outbr *blobref.BlobRef, outerr os.Error) {
	bufr := bufio.NewReader(r)
	spans := []span{} // the tree of spans, cut on interesting rollsum boundaries
	rs := rollsum.New()
	n := int64(0)
	last := n
	buf := new(bytes.Buffer)

	uploadString := func(s string) (*blobref.BlobRef, os.Error) {
		br := blobref.Sha1FromString(s)
		hasIt, err := serverHasBlob(bs, br)
		if err != nil {
			return nil, err
		}
		if hasIt {
			return br, nil
		}
		_, err = bs.ReceiveBlob(br, strings.NewReader(s))
		if err != nil {
			return nil, err
		}
		return br, nil
	}

	// TODO: keep multiple of these in-flight at a time.
	uploadLastSpan := func() bool {
		defer buf.Reset()
		br, err := uploadString(buf.String())
		if err != nil {
			outerr = err
			return false
		}
		spans[len(spans)-1].br = br
		return true
	}

	for {
		c, err := bufr.ReadByte()
		if err == os.EOF {
			if n != last {
				spans = append(spans, span{from: last, to: n})
				if !uploadLastSpan() {
					return
				}
			}
			break
		}
		if err != nil {
			return nil, err
		}
		buf.WriteByte(c)

		n++
		rs.Roll(c)
		if !rs.OnSplit() {
			continue
		}
		bits := rs.Bits()

		// Take any spans from the end of the spans slice that
		// have a smaller 'bits' score and make them children
		// of this node.
		var children []span
		childrenFrom := len(spans)
		for childrenFrom > 0 && spans[childrenFrom-1].bits < bits {
			childrenFrom--
		}
		if nCopy := len(spans) - childrenFrom; nCopy > 0 {
			children = make([]span, nCopy)
			copy(children, spans[childrenFrom:])
			spans = spans[:childrenFrom]
		}

		spans = append(spans, span{from: last, to: n, bits: bits, children: children})
		last = n
		if !uploadLastSpan() {
			return
		}
	}

	var addContentParts func(dst *[]ContentPart, s []span) os.Error

	uploadFile := func(filename string, isFragment bool, fileSize int64, s []span) (*blobref.BlobRef, os.Error) {
		parts := []ContentPart{}
		err := addContentParts(&parts, s)
		if err != nil {
			return nil, err
		}
		m := NewCommonFilenameMap(filename)
		err = PopulateRegularFileMap(m, fileSize, parts)
		if err != nil {
			return nil, err
		}
		if isFragment {
			m["fragment"] = true
		}
		json, err := MapToCamliJson(m)
		if err != nil {
			return nil, err
		}
		return uploadString(json)
	}

	addContentParts = func(dst *[]ContentPart, spansl []span) os.Error {
		for _, sp := range spansl {
			if len(sp.children) > 0 {
				childrenSize := int64(0)
				for _, cs := range sp.children {
					childrenSize += cs.size()
				}
				br, err := uploadFile("", true, childrenSize, sp.children)
				if err != nil {
					return err
				}
				*dst = append(*dst, ContentPart{
					SubBlobRef:       br,
					Size:             uint64(childrenSize),
				})
			}
			if sp.from != sp.to {
				*dst = append(*dst, ContentPart{
					BlobRef:       sp.br,
					Size:          uint64(sp.to - sp.from),
				})
			}
		}
		return nil
	}

	// The top-level content parts
	return uploadFile(filename, false, n, spans)
}
