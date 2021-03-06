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
	"crypto/sha1"
	"fmt"
	"io"
	"json"
	"log"
	"os"
	"rand"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"camli/blobref"
)

var _ = log.Printf

var ErrNoCamliVersion = os.NewError("schema: no camliVersion key in map")
var ErrUnimplemented = os.NewError("schema: unimplemented")

type StatHasher interface {
	Lstat(fileName string) (*os.FileInfo, os.Error)
	Hash(fileName string) (*blobref.BlobRef, os.Error)
}

// Superset represents the superset of common camlistore JSON schema
// keys as a convenient json.Unmarshal target
type Superset struct {
	BlobRef *blobref.BlobRef // Not in JSON, but included for
	// those who want to set it.

	Version int    "camliVersion"
	Type    string "camliType"

	Signer string "camliSigner"
	Sig    string "camliSig"

	ClaimType string "claimType"
	ClaimDate string "claimDate"

	Permanode string "permaNode"
	Attribute string "attribute"
	Value     string "value"

	// TODO: ditch both the FooBytes variants below. a string doesn't have to be UTF-8.

	FileName      string        "fileName"
	FileNameBytes []interface{} "fileNameBytes" // TODO: needs custom UnmarshalJSON?

	SymlinkTarget      string        "symlinkTarget"
	SymlinkTargetBytes []interface{} "symlinkTargetBytes" // TODO: needs custom UnmarshalJSON?

	UnixPermission string "unixPermission"
	UnixOwnerId    int    "unixOwnerId"
	UnixOwner      string "unixOwner"
	UnixGroupId    int    "unixGroupId"
	UnixGroup      string "unixGroup"
	UnixMtime      string "unixMtime"
	UnixCtime      string "unixCtime"
	UnixAtime      string "unixAtime"

	Size         uint64         "size" // for files
	ContentParts []*ContentPart "contentParts"
	Fragment     bool           "fragment"

	Entries string   "entries" // for directories, a blobref to a static-set
	Members []string "members" // for static sets (for directory static-sets:
	// blobrefs to child dirs/files)
}

type ContentPart struct {
	BlobRef          *blobref.BlobRef "blobRef"
	SubBlobRef       *blobref.BlobRef "subFileBlobRef"
	Size             uint64 "size"
	Offset           uint64 "offset"
}

func stringFromMixedArray(parts []interface{}) string {
	buf := new(bytes.Buffer)
	for _, part := range parts {
		if s, ok := part.(string); ok {
			buf.WriteString(s)
			continue
		}
		if num, ok := part.(float64); ok {
			buf.WriteByte(byte(num))
			continue
		}
	}
	return buf.String()
}

func (ss *Superset) SymlinkTargetString() string {
	if ss.SymlinkTarget != "" {
		return ss.SymlinkTarget
	}
	return stringFromMixedArray(ss.SymlinkTargetBytes)
}

func (ss *Superset) FileNameString() string {
	if ss.FileName != "" {
		return ss.FileName
	}
	return stringFromMixedArray(ss.FileNameBytes)
}

func (ss *Superset) HasFilename(name string) bool {
	return ss.FileNameString() == name
}

func (ss *Superset) UnixMode() (mode uint32) {
	m64, err := strconv.Btoui64(ss.UnixPermission, 8)
	if err == nil {
		mode = mode | uint32(m64)
	}

	// TODO: add other types
	switch ss.Type {
	case "directory":
		mode = mode | syscall.S_IFDIR
	case "file":
		mode = mode | syscall.S_IFREG
	case "symlink":
		mode = mode | syscall.S_IFLNK
	}
	return
}

var DefaultStatHasher = &defaultStatHasher{}

type defaultStatHasher struct{}

func (d *defaultStatHasher) Lstat(fileName string) (*os.FileInfo, os.Error) {
	return os.Lstat(fileName)
}

func (d *defaultStatHasher) Hash(fileName string) (*blobref.BlobRef, os.Error) {
	s1 := sha1.New()
	file, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	_, err = io.Copy(s1, file)
	if err != nil {
		return nil, err
	}
	return blobref.FromHash("sha1", s1), nil
}

type StaticSet struct {
	l    sync.Mutex
	refs []*blobref.BlobRef
}

func (ss *StaticSet) Add(ref *blobref.BlobRef) {
	ss.l.Lock()
	defer ss.l.Unlock()
	ss.refs = append(ss.refs, ref)
}

func newCamliMap(version int, ctype string) map[string]interface{} {
	m := make(map[string]interface{})
	m["camliVersion"] = version
	m["camliType"] = ctype
	return m
}

func NewUnsignedPermanode() map[string]interface{} {
	m := newCamliMap(1, "permanode")
	chars := make([]byte, 20)
	// Don't need cryptographically secure random here, as this
	// will be GPG signed anyway.
	rnd := rand.New(rand.NewSource(time.Nanoseconds()))
	for idx, _ := range chars {
		chars[idx] = byte(32 + rnd.Intn(126-32))
	}
	m["random"] = string(chars)
	return m
}

// Map returns a Camli map of camliType "static-set"
func (ss *StaticSet) Map() map[string]interface{} {
	m := newCamliMap(1, "static-set")
	ss.l.Lock()
	defer ss.l.Unlock()

	members := make([]string, 0, len(ss.refs))
	if ss.refs != nil {
		for _, ref := range ss.refs {
			members = append(members, ref.String())
		}
	}
	m["members"] = members
	return m
}

func MapToCamliJson(m map[string]interface{}) (string, os.Error) {
	version, hasVersion := m["camliVersion"]
	if !hasVersion {
		return "", ErrNoCamliVersion
	}
	m["camliVersion"] = 0, false
	jsonBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	m["camliVersion"] = version
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "{\"camliVersion\": %v,\n", version)
	buf.Write(jsonBytes[2:])
	return string(buf.Bytes()), nil
}

func NewCommonFilenameMap(fileName string) map[string]interface{} {
	m := newCamliMap(1, "" /* no type yet */ )
	if fileName != "" {
		lastSlash := strings.LastIndex(fileName, "/")
		baseName := fileName[lastSlash+1:]
		if isValidUtf8(baseName) {
			m["fileName"] = baseName
		} else {
			m["fileNameBytes"] = []uint8(baseName)
		}
	}
	return m
}

func NewCommonFileMap(fileName string, fi *os.FileInfo) map[string]interface{} {
	m := NewCommonFilenameMap(fileName)
	// Common elements (from file-common.txt)
	if !fi.IsSymlink() {
		m["unixPermission"] = fmt.Sprintf("0%o", fi.Permission())
	}
	if fi.Uid != -1 {
		m["unixOwnerId"] = fi.Uid
		if user := getUserFromUid(fi.Uid); user != "" {
			m["unixOwner"] = user
		}
	}
	if fi.Gid != -1 {
		m["unixGroupId"] = fi.Gid
		if group := getGroupFromGid(fi.Gid); group != "" {
			m["unixGroup"] = group
		}
	}
	if mtime := fi.Mtime_ns; mtime != 0 {
		m["unixMtime"] = RFC3339FromNanos(mtime)
	}
	// Include the ctime too, if it differs.
	if ctime := fi.Ctime_ns; ctime != 0 && fi.Mtime_ns != fi.Ctime_ns {
		m["unixCtime"] = RFC3339FromNanos(ctime)
	}

	return m
}

type InvalidContentPartsError struct {
	StatSize   int64
	SumOfParts int64
}

func (e *InvalidContentPartsError) String() string {
	return fmt.Sprintf("Invalid ContentPart slice in PopulateRegularFileMap; file stat size is %d but sum of parts was %d", e.StatSize, e.SumOfParts)
}

func PopulateRegularFileMap(m map[string]interface{}, size int64, parts []ContentPart) os.Error {
	m["camliType"] = "file"
	m["size"] = size

	sumSize := uint64(0)
	mparts := make([]map[string]interface{}, len(parts))
	for idx, part := range parts {
		mpart := make(map[string]interface{})
		mparts[idx] = mpart
		if part.BlobRef != nil {
			mpart["blobRef"] = part.BlobRef.String()
		} else if part.SubBlobRef != nil {
			mpart["subFileBlobRef"] = part.SubBlobRef.String()
		}
		mpart["size"] = part.Size
		sumSize += part.Size
		if part.Offset != 0 {
			mpart["offset"] = part.Offset
		}
	}
	if sumSize != uint64(size) {
		return &InvalidContentPartsError{size, int64(sumSize)}
	}
	m["contentParts"] = mparts
	return nil
}

func PopulateSymlinkMap(m map[string]interface{}, fileName string) os.Error {
	m["camliType"] = "symlink"
	target, err := os.Readlink(fileName)
	if err != nil {
		return err
	}
	if isValidUtf8(target) {
		m["symlinkTarget"] = target
	} else {
		m["symlinkTargetBytes"] = []uint8(target)
	}
	return nil
}

func PopulateDirectoryMap(m map[string]interface{}, staticSetRef *blobref.BlobRef) {
	m["camliType"] = "directory"
	m["entries"] = staticSetRef.String()
}

func NewShareRef(authType string, target *blobref.BlobRef, transitive bool) map[string]interface{} {
	m := newCamliMap(1, "share")
	m["authType"] = authType
	m["target"] = target.String()
	m["transitive"] = transitive
	return m
}

func NewClaim(permaNode *blobref.BlobRef, claimType string) map[string]interface{} {
	m := newCamliMap(1, "claim")
	m["permaNode"] = permaNode.String()
	m["claimType"] = claimType
	m["claimDate"] = RFC3339FromNanos(time.Nanoseconds())
	return m
}

func newAttrChangeClaim(permaNode *blobref.BlobRef, claimType, attr, value string) map[string]interface{} {
	m := NewClaim(permaNode, claimType)
	m["attribute"] = attr
	m["value"] = value
	return m
}

func NewSetAttributeClaim(permaNode *blobref.BlobRef, attr, value string) map[string]interface{} {
	return newAttrChangeClaim(permaNode, "set-attribute", attr, value)
}

func NewAddAttributeClaim(permaNode *blobref.BlobRef, attr, value string) map[string]interface{} {
	return newAttrChangeClaim(permaNode, "add-attribute", attr, value)
}

func NewDelAttributeClaim(permaNode *blobref.BlobRef, attr string) map[string]interface{} {
	m := newAttrChangeClaim(permaNode, "del-attribute", attr, "")
	m["value"] = "", false
	return m
}

// Types of ShareRefs
const ShareHaveRef = "haveref"

func RFC3339FromNanos(epochnanos int64) string {
	nanos := epochnanos % 1e9
	esec := epochnanos / 1e9
	t := time.SecondsToUTC(esec)
	timeStr := t.Format(time.RFC3339)
	if nanos == 0 {
		return timeStr
	}
	nanoStr := fmt.Sprintf("%09d", nanos)
	nanoStr = strings.TrimRight(nanoStr, "0")
	return timeStr[:len(timeStr)-1] + "." + nanoStr + "Z"
}

func NanosFromRFC3339(timestr string) int64 {
	dotpos := strings.Index(timestr, ".")
	simple3339 := timestr
	nanostr := ""
	if dotpos != -1 {
		if !strings.HasSuffix(timestr, "Z") {
			return -1
		}
		simple3339 = timestr[:dotpos] + "Z"
		nanostr = timestr[dotpos+1 : len(timestr)-1]
		if needDigits := 9 - len(nanostr); needDigits > 0 {
			nanostr = nanostr + "000000000"[:needDigits]
		}
	}
	t, err := time.Parse(time.RFC3339, simple3339)
	if err != nil {
		return -1
	}
	nanos, _ := strconv.Atoi64(nanostr)
	return t.Seconds()*1e9 + nanos
}

func populateMap(m map[int]string, file string) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	bufr := bufio.NewReader(f)
	for {
		line, err := bufr.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.Split(line, ":", 4)
		if len(parts) >= 3 {
			idstr := parts[2]
			id, err := strconv.Atoi(idstr)
			if err == nil {
				m[id] = parts[0]
			}
		}
	}
}

var uidToUsernameMap map[int]string
var getUserFromUidOnce sync.Once

func getUserFromUid(uid int) string {
	getUserFromUidOnce.Do(func() {
		uidToUsernameMap = make(map[int]string)
		populateMap(uidToUsernameMap, "/etc/passwd")
	})
	return uidToUsernameMap[uid]
}

var gidToUsernameMap map[int]string
var getGroupFromGidOnce sync.Once

func getGroupFromGid(uid int) string {
	getGroupFromGidOnce.Do(func() {
		gidToUsernameMap = make(map[int]string)
		populateMap(gidToUsernameMap, "/etc/group")
	})
	return gidToUsernameMap[uid]
}

func isValidUtf8(s string) bool {
	for _, rune := range []int(s) {
		if rune == 0xfffd {
			return false
		}
	}
	return true
}
