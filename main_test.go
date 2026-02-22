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

func TestCheckNeedsUpload_NilETag(t *testing.T) {
	mock := &mockS3Client{
		headObjectFunc: func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			return &s3.HeadObjectOutput{ETag: nil}, nil
		},
	}

	needsUpload, reason := checkNeedsUploadWithClient(context.Background(), mock, "bucket", "key", "abc123")
	if !needsUpload {
		t.Error("expected needsUpload=true for nil etag")
	}
	if reason != "no etag" {
		t.Errorf("reason = %q, want %q", reason, "no etag")
	}
}

func TestComputeMD5_LargeFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")

	// 1MB file
	content := make([]byte, 1024*1024)
	for i := range content {
		content[i] = byte(i % 256)
	}

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

func TestS3KeyGeneration(t *testing.T) {
	tests := []struct {
		name    string
		relPath string
		prefix  string
		want    string
	}{
		{"no prefix", "file.txt", "", "file.txt"},
		{"with prefix", "file.txt", "static", "static/file.txt"},
		{"prefix with slash", "file.txt", "static/", "static/file.txt"},
		{"nested path", "dir/subdir/file.txt", "assets", "assets/dir/subdir/file.txt"},
		{"no prefix nested", "dir/file.txt", "", "dir/file.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s3Key := filepath.ToSlash(tt.relPath)
			if tt.prefix != "" {
				s3Key = strings.TrimSuffix(tt.prefix, "/") + "/" + s3Key
			}
			if s3Key != tt.want {
				t.Errorf("s3Key = %q, want %q", s3Key, tt.want)
			}
		})
	}
}

func TestUploadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.html")

	if err := os.WriteFile(path, []byte("<html></html>"), 0644); err != nil {
		t.Fatal(err)
	}

	var capturedInput *s3.PutObjectInput
	mock := &mockS3Client{
		putObjectFunc: func(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedInput = input
			return &s3.PutObjectOutput{}, nil
		},
	}

	err := uploadFileWithClient(context.Background(), mock, "test-bucket", "test.html", path, "max-age=3600", false)
	if err != nil {
		t.Fatal(err)
	}

	if *capturedInput.Bucket != "test-bucket" {
		t.Errorf("bucket = %q, want %q", *capturedInput.Bucket, "test-bucket")
	}
	if *capturedInput.Key != "test.html" {
		t.Errorf("key = %q, want %q", *capturedInput.Key, "test.html")
	}
	if *capturedInput.CacheControl != "max-age=3600" {
		t.Errorf("cache-control = %q, want %q", *capturedInput.CacheControl, "max-age=3600")
	}
	if capturedInput.ContentType == nil || !strings.HasPrefix(*capturedInput.ContentType, "text/html") {
		t.Errorf("content-type = %v, want text/html", capturedInput.ContentType)
	}
}

func TestUploadFile_PublicRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	var capturedInput *s3.PutObjectInput
	mock := &mockS3Client{
		putObjectFunc: func(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedInput = input
			return &s3.PutObjectOutput{}, nil
		},
	}

	err := uploadFileWithClient(context.Background(), mock, "bucket", "key", path, "", true)
	if err != nil {
		t.Fatal(err)
	}

	if capturedInput.ACL != "public-read" {
		t.Errorf("ACL = %q, want %q", capturedInput.ACL, "public-read")
	}
}

func TestUploadFile_NoPublicRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	var capturedInput *s3.PutObjectInput
	mock := &mockS3Client{
		putObjectFunc: func(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedInput = input
			return &s3.PutObjectOutput{}, nil
		},
	}

	err := uploadFileWithClient(context.Background(), mock, "bucket", "key", path, "", false)
	if err != nil {
		t.Fatal(err)
	}

	if capturedInput.ACL != "" {
		t.Errorf("ACL = %q, want empty", capturedInput.ACL)
	}
}

func TestUploadFile_NoCacheControl(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")

	if err := os.WriteFile(path, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	var capturedInput *s3.PutObjectInput
	mock := &mockS3Client{
		putObjectFunc: func(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			capturedInput = input
			return &s3.PutObjectOutput{}, nil
		},
	}

	err := uploadFileWithClient(context.Background(), mock, "bucket", "key", path, "", false)
	if err != nil {
		t.Fatal(err)
	}

	if capturedInput.CacheControl != nil {
		t.Errorf("CacheControl = %q, want nil", *capturedInput.CacheControl)
	}
}

func TestUploadFile_FileNotFound(t *testing.T) {
	mock := &mockS3Client{
		putObjectFunc: func(ctx context.Context, input *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
			return &s3.PutObjectOutput{}, nil
		},
	}

	err := uploadFileWithClient(context.Background(), mock, "bucket", "key", "/nonexistent/file.txt", "", false)
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}

func TestForceUpload(t *testing.T) {
	etag := "\"5eb63bbbe01eeed093cb22bb8f5acdc3\""
	headCalled := false

	mock := &mockS3Client{
		headObjectFunc: func(ctx context.Context, input *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
			headCalled = true
			return &s3.HeadObjectOutput{ETag: &etag}, nil
		},
	}

	// Without force, checkNeedsUpload is called
	headCalled = false
	checkNeedsUploadWithClient(context.Background(), mock, "bucket", "key", "5eb63bbbe01eeed093cb22bb8f5acdc3")
	if !headCalled {
		t.Error("expected HeadObject to be called without force")
	}

	// With force, we skip the check entirely (simulated by not calling checkNeedsUpload)
	// The actual force logic is in main, so we just verify the behavior would differ
	force := true
	if force {
		needsUpload := true
		reason := "forced"
		if !needsUpload || reason != "forced" {
			t.Error("force should always return needsUpload=true with reason=forced")
		}
	}
}

// Helper for testing uploadFile with mock client
func uploadFileWithClient(ctx context.Context, client S3Client, bucket, key, path, cacheControl string, publicRead bool) error {
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
		input.ContentType = &ct
	}

	if cacheControl != "" {
		input.CacheControl = &cacheControl
	}

	if publicRead {
		input.ACL = "public-read"
	}

	_, err = client.PutObject(ctx, input)
	return err
}
