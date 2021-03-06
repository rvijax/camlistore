/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
nYou may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mysqlindexer

import (
	"camli/blobref"

	"log"
	"fmt"
	"os"
	"strings"
)

func (mi *Indexer) Stat(dest chan<- blobref.SizedBlobRef, blobs []*blobref.BlobRef, waitSeconds int) os.Error {
	error := func(err os.Error) os.Error {
		log.Printf("mysqlindexer: stat error: %v", err)
		return err
	}
	// MySQL connection stuff.
	client, err := mi.getConnection()
	if err != nil {
		return error(err)
	}
	defer mi.releaseConnection(client)

	quotedBlobRefs := []string{}
	for _, br := range blobs {
		quotedBlobRefs = append(quotedBlobRefs, fmt.Sprintf("%q", br.String()))
	}
	sql := "SELECT blobref, size FROM blobs WHERE blobref IN (" +
		strings.Join(quotedBlobRefs, ", ") + ")"
	log.Printf("Running: [%s]", sql)
	stmt, err := client.Prepare(sql)
	if err != nil {
		return error(err)
	}
	err = stmt.Execute()
	if err != nil {
		return error(err)
	}

	var row blobRow
	stmt.BindResult(&row.blobref, &row.size)
	for {
		done, err := stmt.Fetch()
		if err != nil {
			return error(err)
		}
		if done {
			break
		}
		br := blobref.Parse(row.blobref)
		if br == nil {
			continue
		}
		dest <- blobref.SizedBlobRef{
			BlobRef: br,
			Size:    row.size,
		}
	}
	return nil
}

