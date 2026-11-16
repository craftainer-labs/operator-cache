//go:build tools

// Package clouddeps pins the cloud SDK modules in go.mod before the cloud
// backends (NKcache-1 and NKcache-2) import them: `go mod tidy` drops a
// requirement nothing imports, and the dependency-only change NKcache-0 must
// keep them and their go.sum entries. The tools build tag keeps this file out
// of every build, test and the coverage gate; tidy still reads it. Each
// backend item deletes its cloud's lines once its code imports the same
// packages, and the last one deletes this package.
//
// Azure is not pinned: cache x Azure is deferred (per-user access strings
// exist only in a preview API).
package clouddeps

import (
	_ "cloud.google.com/go/memorystore/apiv1"
	_ "github.com/aws/aws-sdk-go-v2/service/elasticache"
	_ "github.com/aws/aws-sdk-go-v2/service/memorydb"
)
