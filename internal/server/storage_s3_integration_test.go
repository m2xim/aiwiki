package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const s3IntegrationOperationTimeout = 30 * time.Second

func TestS3CompatibleIntegration(t *testing.T) {
	endpoint := os.Getenv("AIWIKI_TEST_S3_ENDPOINT")
	bucket := os.Getenv("AIWIKI_TEST_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set AIWIKI_TEST_S3_ENDPOINT and AIWIKI_TEST_S3_BUCKET to run the S3 integration test")
	}
	if strings.TrimSpace(endpoint) != endpoint || strings.TrimSpace(bucket) != bucket {
		t.Fatal("S3 integration endpoint and bucket must not have surrounding whitespace")
	}

	region := os.Getenv("AIWIKI_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	if strings.TrimSpace(region) != region {
		t.Fatal("S3 integration region must not have surrounding whitespace")
	}

	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		t.Fatal("could not create a unique S3 integration key")
	}
	prefix := "aiwiki-integration/" + hex.EncodeToString(randomID[:]) + "/"
	key := "fixture"

	initCtx, cancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	store, err := newS3Store(initCtx, S3Config{
		Bucket:                bucket,
		Region:                region,
		Endpoint:              endpoint,
		Prefix:                prefix,
		PathStyle:             true,
		AllowInsecureLoopback: true,
	})
	cancel()
	if err != nil {
		t.Fatal("could not initialize S3 integration store; check the endpoint, bucket, region, and AWS credential chain")
	}

	s3Store, ok := store.(*s3Store)
	if !ok {
		t.Fatal("S3 integration constructor returned an unexpected store implementation")
	}
	cleanupNeeded := false
	t.Cleanup(func() {
		if !cleanupNeeded {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
		defer cleanupCancel()
		if err := store.Delete(cleanupCtx, key); err != nil {
			t.Error("could not clean up the unique S3 integration fixture")
		}
	})

	fixture := []byte("aiwiki S3 integration fixture\n")
	const contentType = "text/plain; charset=utf-8"
	putCtx, putCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	cleanupNeeded = true
	err = store.Put(putCtx, key, bytes.NewReader(fixture), int64(len(fixture)), contentType)
	putCancel()
	if err != nil {
		t.Fatal("S3 integration fixture PUT failed")
	}

	getCtx, getCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	body, err := store.Get(getCtx, key)
	if err != nil {
		getCancel()
		t.Fatal("S3 integration fixture GET failed")
	}
	got, readErr := io.ReadAll(body)
	closeErr := body.Close()
	getCancel()
	if readErr != nil {
		t.Fatal("could not read the complete S3 integration fixture")
	}
	if closeErr != nil {
		t.Fatal("could not close the S3 integration response body")
	}
	if !bytes.Equal(got, fixture) {
		t.Fatal("S3 integration fixture bytes did not round-trip exactly")
	}

	headCtx, headCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	head, err := s3Store.client.HeadObject(headCtx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(prefix + key),
	})
	headCancel()
	if err != nil {
		t.Fatal("S3 integration fixture metadata HEAD failed")
	}
	if aws.ToString(head.ContentType) != contentType {
		t.Fatalf("S3 integration fixture MIME type mismatch: got %q", aws.ToString(head.ContentType))
	}

	missingCtx, missingCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	missingBody, err := store.Get(missingCtx, "missing-fixture")
	if missingBody != nil {
		_ = missingBody.Close()
	}
	missingCancel()
	if !errors.Is(err, errObjectNotFound) {
		t.Fatal("S3 integration missing-key GET did not return the object-not-found error")
	}

	deleteCtx, deleteCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	err = store.Delete(deleteCtx, key)
	deleteCancel()
	if err != nil {
		t.Fatal("S3 integration fixture DELETE failed")
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), s3IntegrationOperationTimeout)
	deletedBody, err := store.Get(verifyCtx, key)
	if deletedBody != nil {
		_ = deletedBody.Close()
	}
	verifyCancel()
	if !errors.Is(err, errObjectNotFound) {
		t.Fatal("S3 integration deleted fixture remains readable")
	}
	cleanupNeeded = false
}
