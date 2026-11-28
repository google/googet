/*
Copyright 2026 Google Inc. All Rights Reserved.
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

package cli

import (
	"context"

	"github.com/google/googet/v2/client"
	"github.com/google/googet/v2/googetdb"
	"github.com/google/googet/v2/settings"
)

// The factories in this file are the only way that commands get the googet
// database and a Downloader, so that whether a command can write the database
// or the repo index cache follows from whether the run is privileged.
// Commands call them only from Execute and pass the results to the functions
// that need them. A guard test fails on any other use of googetdb.NewDB,
// googetdb.OpenReadOnly, googetdb.CreateIfMissing, client.NewDownloader, a
// client.Downloader value or its ReadOnlyCache field in this package and its
// subpackages.
//
// The factories protect only the database and the index cache. Package
// downloads, repo files and the cache's package files aren't covered: what
// stops a command from writing those in an unelevated run is
// RefuseUnlessAllowed or RefuseUnlessPrivileged as the first statement of its
// Execute.

// OpenDB opens the googet database. In a privileged run it opens the database
// read-write with googetdb.NewDB, which creates the database if it is
// missing. Otherwise it opens the existing database with
// googetdb.OpenReadOnly, so a command that is wrongly allowed to run
// unelevated gets an error when it writes, rather than a write.
func OpenDB(ctx context.Context) (*googetdb.GooDB, error) {
	if IsPrivileged(ctx) {
		return googetdb.NewDB(settings.DBFile())
	}
	return googetdb.OpenReadOnly(settings.DBFile())
}

// NewDownloader returns a Downloader that uses proxyServer, if it isn't
// empty. Outside a privileged run the Downloader's ReadOnlyCache is set, so
// it never writes the repo index cache.
func NewDownloader(ctx context.Context, proxyServer string) (*client.Downloader, error) {
	d, err := client.NewDownloader(proxyServer)
	if err != nil {
		return nil, err
	}
	d.ReadOnlyCache = !IsPrivileged(ctx)
	return d, nil
}
