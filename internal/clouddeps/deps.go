//go:build tools

// Package clouddeps pins the cloud SDK modules in go.mod before the cloud
// backend that imports them (NKcache-2) exists: `go mod tidy` drops a
// requirement nothing imports. The tools build tag keeps this file out of every
// build, test and the coverage gate; tidy still reads it. NKcache-2 deletes the
// Memorystore line once its code imports the same package, and then this
// package. The AWS modules left when NKcache-1 imported them.
//
// Azure is not pinned: cache x Azure is deferred (per-user access strings
// exist only in a preview API).
package clouddeps

import (
	_ "cloud.google.com/go/memorystore/apiv1"
)
