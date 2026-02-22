# s3push

Recursively upload files to S3, skipping files that already exist with matching MD5.

## Install

```bash
go install github.com/mrkamel/s3push@latest
```

Or build from source:

```bash
go build -o s3push .
```

## Usage

```bash
s3push -src <directory> -bucket <bucket> [options]
```

### Options

| Flag | Description | Default |
|------|-------------|---------|
| `-src` | Source directory to upload | (required) |
| `-bucket` | S3 bucket name | (required) |
| `-prefix` | S3 key prefix | |
| `-cache-control` | Cache-Control header | |
| `-concurrency` | Number of concurrent uploads | 10 |
| `-dry-run` | Show what would be uploaded | false |
| `-verbose` | Verbose output | false |

### Examples

```bash
# Preview uploads
s3push -src ./dist -bucket my-bucket -dry-run

# Upload with cache headers
s3push -src ./dist -bucket my-bucket -cache-control "max-age=31536000"

# Upload to a prefix
s3push -src ./dist -bucket my-bucket -prefix "static/v1"

# Faster uploads
s3push -src ./dist -bucket my-bucket -concurrency 50
```

## How it works

For each local file, s3push:

1. Computes the local MD5 hash
2. Fetches the S3 object's ETag (which is MD5 for single-part uploads)
3. Uploads only if the file is new or the hash differs

Content-Type is set automatically based on file extension.

## Exit codes

- `0` - Success
- `1` - One or more errors occurred
