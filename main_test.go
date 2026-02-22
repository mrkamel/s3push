package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

func TestComputeMD5(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	content := []byte("hello world")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := computeMD5(path)
	if err != nil {
		t.Fatal(err)
	}

	h := md5.Sum(content)
	want := hex.EncodeToString(h[:])

	if got != want {
		t.Errorf("computeMD5() = %q, want %q", got, want)
	}
}

func TestComputeMD5_NotFound(t *testing.T) {
	_, err := computeMD5("/nonexistent/file.txt")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestComputeMD5_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")

	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	got, err := computeMD5(path)
	if err != nil {
		t.Fatal(err)
	}

	h := md5.Sum([]byte{})
	want := hex.EncodeToString(h[:])

	if got != want {
		t.Errorf("computeMD5() = %q, want %q", got, want)
	}
}

// Mock S3 client for testing
type mockS3Client struct {
	headObjectFunc func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	putObjectFunc  func(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

func (m *mockS3Client) HeadObject(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return m.headObjectFunc(ctx, input, opts...)
}

func (m *mockS3Client) PutObject(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return m.putObjectFunc(ctx, input, opts...)
}

// S3Client interface for testing
type S3Client interface {
	HeadObject(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	PutObject(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

func checkNeedsUploadWithClient(ctx context.Context, client S3Client, bucket, key, localMD5 string) (bool, string) {
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &bucket,
		Key:    &key,
	})

	if err != nil {
		return true, "new file"
	}

	if head.ETag == nil {
		return true, "no etag"
	}

	remoteETag := (*head.ETag)[1 : len(*head.ETag)-1] // trim quotes

	if len(remoteETag) > 32 {
		return true, "multipart etag"
	}

	if remoteETag != localMD5 {
		return true, "md5 mismatch"
	}

	return false, ""
}

func TestCheckNeedsUpload_NewFile(t *testing.T) {
	mock := &mockS3Client{
		headObjectFunc: func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return nil, &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"}
		},
	}

	needsUpload, reason := checkNeedsUploadWithClient(context.Background(), mock, "bucket", "key", "abc123")
	if !needsUpload {
		t.Error("expected needsUpload=true for new file")
	}
	if reason != "new file" {
		t.Errorf("reason = %q, want %q", reason, "new file")
	}
}

func TestCheckNeedsUpload_Unchanged(t *testing.T) {
	etag := "\"5eb63bbbe01eeed093cb22bb8f5acdc3\""
	mock := &mockS3Client{
		headObjectFunc: func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ETag: &etag}, nil
		},
	}

	needsUpload, _ := checkNeedsUploadWithClient(context.Background(), mock, "bucket", "key", "5eb63bbbe01eeed093cb22bb8f5acdc3")
	if needsUpload {
		t.Error("expected needsUpload=false for unchanged file")
	}
}

func TestCheckNeedsUpload_MD5Mismatch(t *testing.T) {
	etag := "\"5eb63bbbe01eeed093cb22bb8f5acdc3\""
	mock := &mockS3Client{
		headObjectFunc: func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ETag: &etag}, nil
		},
	}

	needsUpload, reason := checkNeedsUploadWithClient(context.Background(), mock, "bucket", "key", "differentmd5hash")
	if !needsUpload {
		t.Error("expected needsUpload=true for md5 mismatch")
	}
	if reason != "md5 mismatch" {
		t.Errorf("reason = %q, want %q", reason, "md5 mismatch")
	}
}

func TestCheckNeedsUpload_MultipartETag(t *testing.T) {
	etag := "\"5eb63bbbe01eeed093cb22bb8f5acdc3-2\""
	mock := &mockS3Client{
		headObjectFunc: func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ETag: &etag}, nil
		},
	}

	needsUpload, reason := checkNeedsUploadWithClient(context.Background(), mock, "bucket", "key", "5eb63bbbe01eeed093cb22bb8f5acdc3")
	if !needsUpload {
		t.Error("expected needsUpload=true for multipart etag")
	}
	if reason != "multipart etag" {
		t.Errorf("reason = %q, want %q", reason, "multipart etag")
	}
}

func TestContentType(t *testing.T) {
	tests := []struct {
		ext  string
		want string
	}{
		{".html", "text/html"},
		{".css", "text/css"},
		{".js", "text/javascript"},
		{".json", "application/json"},
		{".png", "image/png"},
		{".jpg", "image/jpeg"},
		{".svg", "image/svg+xml"},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			got := mime.TypeByExtension(tt.ext)
			if got == "" {
				t.Skipf("mime type for %s not available on this system", tt.ext)
			}
			// Check prefix since charset may be appended
			if got != tt.want && !strings.HasPrefix(got, tt.want) {
				t.Errorf("mime.TypeByExtension(%q) = %q, want %q", tt.ext, got, tt.want)
			}
		})
	}
}
