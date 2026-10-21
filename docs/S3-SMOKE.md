# S3-compatible storage smoke test

`TestS3CompatibleIntegration` exercises the normal AWS SDK credential chain and
the production `newS3Store` constructor against an already-existing S3 or
S3-compatible bucket. It never creates a bucket or changes bucket settings.

The test uses a fresh `aiwiki-integration/<random>/fixture` key, writes a small
plain-text fixture, checks its exact bytes and MIME type, checks the missing-key
error, and deletes that fixture. Cleanup targets only that same unique key. Each
service operation has a 30-second deadline. The test does not list a bucket or
request public or presigned URLs. It does not print the endpoint or credential
values.

## Run locally

Use an existing, disposable test bucket to which your AWS credential chain has
access. Credentials come from the standard AWS SDK chain, such as your local
AWS profile or environment; do not put credentials in this document or in the
endpoint URL. Do not point this test at a production bucket.

PowerShell example for a local S3-compatible service already running on your
machine (the test itself does not start containers):

```powershell
$env:AIWIKI_TEST_S3_ENDPOINT = "http://127.0.0.1:9000"
$env:AIWIKI_TEST_S3_BUCKET = "aiwiki-test"
$env:AIWIKI_TEST_S3_REGION = "us-east-1"
go test ./internal/server -run '^TestS3CompatibleIntegration$' -count=1
```

For AWS S3, set `AIWIKI_TEST_S3_ENDPOINT` to the HTTPS endpoint for the intended
region and set `AIWIKI_TEST_S3_BUCKET` to the existing test bucket. The region
defaults to `us-east-1` when `AIWIKI_TEST_S3_REGION` is unset. The test uses
path-style addressing. Plain HTTP is accepted only for loopback endpoints and
is explicitly opted into by the test configuration.

Unset the test variables when finished:

```powershell
Remove-Item Env:AIWIKI_TEST_S3_ENDPOINT -ErrorAction SilentlyContinue
Remove-Item Env:AIWIKI_TEST_S3_BUCKET -ErrorAction SilentlyContinue
Remove-Item Env:AIWIKI_TEST_S3_REGION -ErrorAction SilentlyContinue
```

## Interpreting the result

If either the endpoint or bucket variable is absent, Go reports the test as
skipped. A skipped result confirms only that the test is opt-in; it provides no
service coverage. An actual pass requires a configured endpoint and bucket and
successful requests through the AWS SDK credential chain. This document does
not claim that the test has been run against a service.
