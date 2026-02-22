package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type uploadJob struct {
	path    string
	relPath string
	s3Key   string
}

func main() {
	var (
		srcDir       string
		bucket       string
		prefix       string
		cacheControl string
		publicRead   bool
		force        bool
		dryRun       bool
		verbose      bool
		concurrency  int
	)

	flag.StringVar(&srcDir, "src", "", "Source directory to upload")
	flag.StringVar(&bucket, "bucket", "", "S3 bucket name")
	flag.StringVar(&prefix, "prefix", "", "S3 key prefix (optional)")
	flag.StringVar(&cacheControl, "cache-control", "", "Cache-Control header value")
	flag.BoolVar(&publicRead, "public-read", false, "Set ACL to public-read")
	flag.BoolVar(&force, "force", false, "Upload all files even if unchanged")
	flag.BoolVar(&dryRun, "dry-run", false, "Show what would be uploaded without uploading")
	flag.BoolVar(&verbose, "verbose", false, "Verbose output")
	flag.IntVar(&concurrency, "concurrency", 10, "Number of concurrent uploads")
	flag.Parse()

	if srcDir == "" || bucket == "" {
		fmt.Fprintln(os.Stderr, "Usage: s3push -src <directory> -bucket <bucket> [-prefix <prefix>] [-dry-run] [-verbose]")
		flag.PrintDefaults()
		os.Exit(1)
	}

	srcDir, err := filepath.Abs(srcDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving source path: %v\n", err)
		os.Exit(1)
	}

	info, err := os.Stat(srcDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error accessing source directory: %v\n", err)
		os.Exit(1)
	}
	if !info.IsDir() {
		fmt.Fprintf(os.Stderr, "Source must be a directory: %s\n", srcDir)
		os.Exit(1)
	}

	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading AWS config: %v\n", err)
		os.Exit(1)
	}

	client := s3.NewFromConfig(cfg)

	var (
		checked  atomic.Int64
		uploaded atomic.Int64
		skipped  atomic.Int64
		errors   atomic.Int64
	)

	jobs := make(chan uploadJob)
	var wg sync.WaitGroup

	// Start workers
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				var needsUpload bool
				var reason string

				if force {
					needsUpload = true
					reason = "forced"
				} else {
					localMD5, err := computeMD5(job.path)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error computing MD5 for %s: %v\n", job.path, err)
						errors.Add(1)
						continue
					}

					needsUpload, reason = checkNeedsUpload(ctx, client, bucket, job.s3Key, localMD5)
				}

				if !needsUpload {
					if verbose {
						fmt.Printf("SKIP %s (unchanged)\n", job.relPath)
					}
					skipped.Add(1)
					continue
				}

				if dryRun {
					fmt.Printf("WOULD UPLOAD %s -> s3://%s/%s (%s)\n", job.relPath, bucket, job.s3Key, reason)
					uploaded.Add(1)
					continue
				}

				if verbose {
					fmt.Printf("UPLOAD %s -> s3://%s/%s (%s)\n", job.relPath, bucket, job.s3Key, reason)
				} else {
					fmt.Printf("UPLOAD %s\n", job.relPath)
				}

				if err := uploadFile(ctx, client, bucket, job.s3Key, job.path, cacheControl, publicRead); err != nil {
					fmt.Fprintf(os.Stderr, "Error uploading %s: %v\n", job.path, err)
					errors.Add(1)
					continue
				}

				uploaded.Add(1)
			}
		}()
	}

	// Walk and send jobs
	err = filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error accessing %s: %v\n", path, err)
			errors.Add(1)
			return nil
		}

		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(srcDir, path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting relative path for %s: %v\n", path, err)
			errors.Add(1)
			return nil
		}

		s3Key := filepath.ToSlash(relPath)
		if prefix != "" {
			s3Key = strings.TrimSuffix(prefix, "/") + "/" + s3Key
		}

		checked.Add(1)
		jobs <- uploadJob{path: path, relPath: relPath, s3Key: s3Key}
		return nil
	})

	close(jobs)
	wg.Wait()

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error walking directory: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSummary: %d checked, %d uploaded, %d skipped, %d errors\n",
		checked.Load(), uploaded.Load(), skipped.Load(), errors.Load())

	if errors.Load() > 0 {
		os.Exit(1)
	}
}

func computeMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func checkNeedsUpload(ctx context.Context, client *s3.Client, bucket, key, localMD5 string) (bool, string) {
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})

	if err != nil {
		// Object doesn't exist or other error - needs upload
		return true, "new file"
	}

	if head.ETag == nil {
		return true, "no etag"
	}

	// ETag is quoted and for single-part uploads is the MD5
	remoteETag := strings.Trim(*head.ETag, "\"")

	// For multipart uploads, ETag contains a dash - we need to re-upload
	if strings.Contains(remoteETag, "-") {
		return true, "multipart etag"
	}

	if remoteETag != localMD5 {
		return true, "md5 mismatch"
	}

	return false, ""
}

func uploadFile(ctx context.Context, client *s3.Client, bucket, key, path, cacheControl string, publicRead bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	input := &s3.PutObjectInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   f,
	}

	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		input.ContentType = aws.String(ct)
	}

	if cacheControl != "" {
		input.CacheControl = aws.String(cacheControl)
	}

	if publicRead {
		input.ACL = types.ObjectCannedACLPublicRead
	}

	_, err = client.PutObject(ctx, input)
	return err
}
