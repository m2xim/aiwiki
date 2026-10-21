package server

import (
	"context"
	"io"
)

// objectStore never returns public or presigned URLs; reads stay behind ACL checks.
type objectStore interface {
	Put(context.Context, string, io.ReadSeeker, int64, string) error
	Get(context.Context, string) (io.ReadCloser, error)
	Delete(context.Context, string) error
}

type S3Config struct {
	Bucket                string
	Region                string
	Endpoint              string
	Prefix                string
	PathStyle             bool
	AllowInsecureLoopback bool
}
