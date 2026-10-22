package server

import (
	"context"
	"io"
	"time"
)

// objectStore never returns public or presigned URLs; reads stay behind ACL checks.
type objectStore interface {
	Put(context.Context, string, io.ReadSeeker, int64, string) error
	Get(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
	// List returns the objects whose key (relative to the store prefix) starts
	// with the given prefix. Only the offline storage check uses it.
	List(context.Context, string) ([]objectInfo, error)
}

// objectInfo describes one stored object; Key is relative to the store prefix.
type objectInfo struct {
	Key      string
	Size     int64
	Modified time.Time
}

type S3Config struct {
	Bucket                string
	Region                string
	Endpoint              string
	Prefix                string
	PathStyle             bool
	AllowInsecureLoopback bool
}
