package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

const (
	s3StartupTimeout = 10 * time.Second
	s3PutTimeout     = 5 * time.Minute
	s3GetTimeout     = 5 * time.Minute
	s3DeleteTimeout  = 30 * time.Second
	s3ListTimeout    = time.Minute // per page
	s3MaxAttempts    = 3
	s3MaxKeyBytes    = 900
)

var (
	errObjectNotFound = errors.New("object not found")
	errNoObjectStore  = errors.New("object storage is not configured")
)

// validateS3Prefix returns the prefix normalized to "" or "a/b/" form.
func validateS3Prefix(prefix string) (string, error) {
	if prefix == "" {
		return "", nil
	}
	if strings.HasPrefix(prefix, "/") || strings.Contains(prefix, "\\") || len(prefix) > 256 {
		return "", errors.New("invalid S3 prefix")
	}
	for _, c := range prefix {
		if c < ' ' || c == 0x7f {
			return "", errors.New("invalid S3 prefix")
		}
	}
	trimmed := strings.TrimSuffix(prefix, "/")
	if trimmed == "" {
		return "", errors.New("invalid S3 prefix")
	}
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New("invalid S3 prefix")
		}
	}
	return trimmed + "/", nil
}

// validObjectKey accepts a single opaque name: no separators, dot segments or control characters.
func validObjectKey(key string) bool {
	if key == "" || key == "." || key == ".." || len(key) > 255 {
		return false
	}
	for _, c := range key {
		if c < ' ' || c == 0x7f || c == '/' || c == '\\' {
			return false
		}
	}
	return true
}

func validateS3Endpoint(cfg S3Config) (string, error) {
	if cfg.Endpoint == "" {
		return "", nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return "", errors.New("invalid S3 endpoint")
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !cfg.AllowInsecureLoopback || !isLoopbackHost(u.Hostname()) {
			return "", errors.New("S3 endpoint must use HTTPS (plain HTTP only for loopback with explicit opt-in)")
		}
	default:
		return "", errors.New("invalid S3 endpoint scheme")
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, nil
}

type s3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// newS3Store builds an S3 (or S3-compatible) store using the default AWS
// credential chain and verifies the bucket with HEAD; it never creates or
// reconfigures anything. Any failure is fatal to the caller (fail closed).
func newS3Store(ctx context.Context, cfg S3Config) (objectStore, error) {
	if cfg.Region == "" {
		cfg.Region = os.Getenv("AWS_REGION")
		if cfg.Region == "" {
			cfg.Region = os.Getenv("AWS_DEFAULT_REGION")
		}
	}
	if strings.TrimSpace(cfg.Bucket) == "" || strings.TrimSpace(cfg.Region) == "" {
		return nil, errors.New("S3 bucket and region are required")
	}
	prefix, err := validateS3Prefix(cfg.Prefix)
	if err != nil {
		return nil, err
	}
	endpoint, err := validateS3Endpoint(cfg)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	loadCtx, cancel := context.WithTimeout(ctx, s3StartupTimeout)
	defer cancel()
	awsCfg, err := config.LoadDefaultConfig(loadCtx,
		config.WithRegion(cfg.Region),
		config.WithHTTPClient(httpClient),
		config.WithRetryMaxAttempts(s3MaxAttempts),
	)
	if err != nil {
		return nil, errors.New("could not load AWS configuration")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.PathStyle
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
		// Many S3-compatible servers reject the optional trailing checksums.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	if _, err = client.HeadBucket(loadCtx, &s3.HeadBucketInput{Bucket: aws.String(cfg.Bucket)}); err != nil {
		return nil, fmt.Errorf("S3 bucket check failed: %s", s3ErrorSummary(err))
	}
	return &s3Store{client: client, bucket: cfg.Bucket, prefix: prefix}, nil
}

// s3ErrorSummary reports only an API error code, never request details.
func s3ErrorSummary(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() != "" {
		return api.ErrorCode()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "request failed"
}

func (s *s3Store) objectKey(key string) (*string, error) {
	if !validObjectKey(key) || len(s.prefix)+len(key) > s3MaxKeyBytes {
		return nil, errors.New("invalid object key")
	}
	return aws.String(s.prefix + key), nil
}

func (s *s3Store) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, contentType string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if size < 0 {
		return errors.New("invalid object size")
	}
	ctx, cancel := context.WithTimeout(ctx, s3PutTimeout)
	defer cancel()
	in := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: k, Body: body, ContentLength: aws.Int64(size)}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	if _, err = s.client.PutObject(ctx, in); err != nil {
		return fmt.Errorf("S3 put failed: %s", s3ErrorSummary(err))
	}
	return nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s3GetTimeout)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: k})
	if err != nil {
		cancel()
		var api smithy.APIError
		if errors.As(err, &api) && (api.ErrorCode() == "NoSuchKey" || api.ErrorCode() == "NotFound") {
			return nil, errObjectNotFound
		}
		return nil, fmt.Errorf("S3 get failed: %s", s3ErrorSummary(err))
	}
	return cancelOnClose{ReadCloser: out.Body, cancel: cancel}, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s3DeleteTimeout)
	defer cancel()
	if _, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: k}); err != nil {
		return fmt.Errorf("S3 delete failed: %s", s3ErrorSummary(err))
	}
	return nil
}

// localStore keeps objects as files in one directory (legacy single-user mode).
type localStore struct{ dir string }

func (l localStore) path(key string) (string, error) {
	if !validObjectKey(key) {
		return "", errors.New("invalid object key")
	}
	return filepath.Join(l.dir, key), nil
}

func (l localStore) Put(ctx context.Context, key string, body io.ReadSeeker, size int64, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(body, size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != size {
		err = errors.New("object size mismatch")
	}
	if err != nil {
		_ = os.Remove(p)
	}
	return err
}

func (l localStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l localStore) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err = os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// objectStorage returns the configured store, else the local files directory
// for the legacy edition. Corporate (PostgreSQL) never falls back to disk.
func (s *Server) objectStorage() (objectStore, error) {
	if s.objects != nil {
		return s.objects, nil
	}
	if s.DB != nil && s.DB.pg {
		return nil, errNoObjectStore
	}
	return localStore{dir: filepath.Join(s.DataDir, "files")}, nil
}

// sniffMIME detects the content type from the first 512 bytes and rewinds f.
func sniffMIME(f io.ReadSeeker) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, 512)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return http.DetectContentType(buf[:n]), nil
}

// List pages through ListObjectsV2 under the store prefix plus the key prefix.
func (s *s3Store) List(ctx context.Context, prefix string) ([]objectInfo, error) {
	if strings.ContainsAny(prefix, "\\x00") || len(s.prefix)+len(prefix) > s3MaxKeyBytes {
		return nil, errors.New("invalid object prefix")
	}
	var out []objectInfo
	var token *string
	for {
		pctx, cancel := context.WithTimeout(ctx, s3ListTimeout)
		page, err := s.client.ListObjectsV2(pctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix + prefix), ContinuationToken: token})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("S3 list failed: %s", s3ErrorSummary(err))
		}
		for _, o := range page.Contents {
			info := objectInfo{Key: strings.TrimPrefix(aws.ToString(o.Key), s.prefix), Size: aws.ToInt64(o.Size)}
			if o.LastModified != nil {
				info.Modified = *o.LastModified
			}
			if info.Key != "" {
				out = append(out, info)
			}
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// List walks the (flat) files directory.
func (l localStore) List(ctx context.Context, prefix string) ([]objectInfo, error) {
	entries, err := os.ReadDir(l.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []objectInfo
	for _, e := range entries {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if !e.Type().IsRegular() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, objectInfo{Key: e.Name(), Size: fi.Size(), Modified: fi.ModTime()})
	}
	return out, nil
}
