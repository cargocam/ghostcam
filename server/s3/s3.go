// Package s3 provides S3/Tigris presigned URL generation.
package s3

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Client wraps an S3 client for presigned URL generation.
type Client struct {
	client     *s3.Client
	presigner  *s3.PresignClient
	bucket     string
	presignTTL time.Duration
}

// New creates a new S3 client.
func New(ctx context.Context, bucket, region, endpoint string, presignTTLSecs uint64) (*Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	if endpoint != "" {
		opts = append(opts, awsconfig.WithBaseEndpoint(endpoint))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	return &Client{
		client:     client,
		presigner:  s3.NewPresignClient(client),
		bucket:     bucket,
		presignTTL: time.Duration(presignTTLSecs) * time.Second,
	}, nil
}

// PresignPut generates a presigned PUT URL for the given S3 key.
func (c *Client) PresignPut(ctx context.Context, key string) (string, error) {
	req, err := c.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(c.presignTTL))
	if err != nil {
		return "", fmt.Errorf("presigning PUT: %w", err)
	}
	return req.URL, nil
}

// PresignGet generates a presigned GET URL for the given S3 key.
func (c *Client) PresignGet(ctx context.Context, key string) (string, error) {
	req, err := c.presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(c.presignTTL))
	if err != nil {
		return "", fmt.Errorf("presigning GET: %w", err)
	}
	return req.URL, nil
}

// Upload puts an object directly into S3.
func (c *Client) Upload(ctx context.Context, key string, data []byte, contentType string) error {
	_, err := c.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("uploading to S3: %w", err)
	}
	return nil
}

// Delete removes an object from S3 by key.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("deleting S3 object %s: %w", key, err)
	}
	return nil
}

// FirmwareKey returns the S3 key for a firmware binary.
func FirmwareKey(version string) string {
	return fmt.Sprintf("firmware/%s/ghostcam-camera", version)
}

// PresignTTLSecs returns the presign TTL in seconds.
func (c *Client) PresignTTLSecs() uint64 {
	return uint64(c.presignTTL.Seconds())
}

// InitKey returns the S3 key for a camera's init segment.
func InitKey(deviceID string) string {
	return fmt.Sprintf("%s/init.mp4", deviceID)
}

// SegmentKey returns the S3 key for a camera segment.
func SegmentKey(deviceID, segmentID string) string {
	return fmt.Sprintf("%s/%s.ts", deviceID, segmentID)
}
