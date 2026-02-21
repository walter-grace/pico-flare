// Package storage provides R2 (S3-compatible) object storage via AWS SDK v2.
// Endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
package storage

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2Client is an S3-compatible client for Cloudflare R2.
type R2Client struct {
	client *s3.Client
}

// NewR2Client creates an R2 client with the given account ID and R2 API credentials.
// Uses endpoint https://<accountID>.r2.cloudflarestorage.com and region "auto".
func NewR2Client(accountID, accessKeyID, secretAccessKey string) (*R2Client, error) {
	if accountID == "" || accessKeyID == "" || secretAccessKey == "" {
		return nil, fmt.Errorf("accountID, accessKeyID, and secretAccessKey are required")
	}
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)

	cfg := aws.Config{
		Region: "auto",
		Credentials: credentials.NewStaticCredentialsProvider(
			accessKeyID,
			secretAccessKey,
			"",
		),
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &R2Client{client: client}, nil
}

// UploadObject uploads data to the given bucket and key.
func (c *R2Client) UploadObject(ctx context.Context, bucket, key string, data []byte) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	return err
}

// DownloadObject downloads the object at the given bucket and key.
func (c *R2Client) DownloadObject(ctx context.Context, bucket, key string) ([]byte, error) {
	out, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()

	var buf bytes.Buffer
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
