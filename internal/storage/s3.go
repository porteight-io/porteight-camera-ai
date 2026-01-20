package storage

import (
	"context"
	"errors"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type S3Config struct {
	Region string
	Bucket string
	Prefix string // optional; e.g. "porteight-camera-ai"

	// Server-side encryption (recommended)
	// - "" => none
	// - "AES256" => SSE-S3
	// - "aws:kms" => SSE-KMS (requires KMSKeyID)
	SSE      string
	KMSKeyID string

	// Presign URL TTL for segment access
	PresignTTL time.Duration
}

type S3Store struct {
	cfg      S3Config
	client   *s3.Client
	uploader *manager.Uploader
	presign  *s3.PresignClient
}

func NewS3Store(ctx context.Context, cfgIn S3Config) (*S3Store, error) {
	if cfgIn.Bucket == "" {
		return nil, errors.New("S3 bucket is required")
	}
	if cfgIn.Region == "" {
		cfgIn.Region = os.Getenv("AWS_REGION")
	}
	if cfgIn.Region == "" {
		cfgIn.Region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if cfgIn.Region == "" {
		return nil, errors.New("S3 region is required (set AWS_REGION)")
	}
	if cfgIn.PresignTTL == 0 {
		cfgIn.PresignTTL = 5 * time.Minute
	}

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfgIn.Region))
	if err != nil {
		return nil, err
	}
	client := s3.NewFromConfig(awsCfg)
	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.Concurrency = 8
	})
	return &S3Store{
		cfg:      cfgIn,
		client:   client,
		uploader: uploader,
		presign:  s3.NewPresignClient(client),
	}, nil
}

func (s *S3Store) Bucket() string { return s.cfg.Bucket }

func (s *S3Store) joinPrefix(parts ...string) string {
	p := strings.Trim(s.cfg.Prefix, "/")
	all := make([]string, 0, 1+len(parts))
	if p != "" {
		all = append(all, p)
	}
	for _, x := range parts {
		x = strings.Trim(x, "/")
		if x != "" {
			all = append(all, x)
		}
	}
	return strings.Join(all, "/")
}

func (s *S3Store) UploadDir(ctx context.Context, localDir string, destPrefix string) (string, error) {
	if localDir == "" {
		return "", errors.New("localDir required")
	}
	fi, err := os.Stat(localDir)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", errors.New("localDir must be a directory")
	}

	destPrefix = strings.Trim(destPrefix, "/")
	fullPrefix := s.joinPrefix(destPrefix)

	var uploadErr error
	_ = filepath.WalkDir(localDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			uploadErr = err
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			uploadErr = err
			return err
		}
		rel = filepath.ToSlash(rel)
		key := fullPrefix
		if key != "" {
			key = key + "/" + rel
		} else {
			key = rel
		}

		f, err := os.Open(path)
		if err != nil {
			uploadErr = err
			return err
		}
		defer f.Close()

		ct := mime.TypeByExtension(filepath.Ext(path))
		if ct == "" {
			// HLS typical types
			switch strings.ToLower(filepath.Ext(path)) {
			case ".m3u8":
				ct = "application/vnd.apple.mpegurl"
			case ".ts":
				ct = "video/MP2T"
			default:
				ct = "application/octet-stream"
			}
		}

		input := &s3.PutObjectInput{
			Bucket:      aws.String(s.cfg.Bucket),
			Key:         aws.String(key),
			Body:        f,
			ContentType: aws.String(ct),
		}

		// Enable server-side encryption when configured
		switch s.cfg.SSE {
		case "AES256":
			input.ServerSideEncryption = types.ServerSideEncryptionAes256
		case "aws:kms":
			input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
			if s.cfg.KMSKeyID != "" {
				input.SSEKMSKeyId = aws.String(s.cfg.KMSKeyID)
			}
		}

		_, err = s.uploader.Upload(ctx, input)
		if err != nil {
			uploadErr = err
			return err
		}
		return nil
	})

	if uploadErr != nil {
		return fullPrefix, uploadErr
	}
	return fullPrefix, nil
}

func (s *S3Store) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (s *S3Store) PresignGet(ctx context.Context, key string) (string, error) {
	res, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.cfg.Bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(s.cfg.PresignTTL))
	if err != nil {
		return "", err
	}
	return res.URL, nil
}

func (s *S3Store) RecordingPrefixForSession(streamKey, session string) string {
	// recordings/<key>/<session>/...
	return s.joinPrefix("recordings", streamKey, session)
}

// UploadFile uploads a single local file to a specific S3 object key.
// key must be the full object key (including any configured Prefix).
func (s *S3Store) UploadFile(ctx context.Context, localPath string, key string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	ct := mime.TypeByExtension(filepath.Ext(localPath))
	if ct == "" {
		switch strings.ToLower(filepath.Ext(localPath)) {
		case ".m3u8":
			ct = "application/vnd.apple.mpegurl"
		case ".ts":
			ct = "video/MP2T"
		default:
			ct = "application/octet-stream"
		}
	}

	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.cfg.Bucket),
		Key:         aws.String(key),
		Body:        f,
		ContentType: aws.String(ct),
	}

	switch s.cfg.SSE {
	case "AES256":
		input.ServerSideEncryption = types.ServerSideEncryptionAes256
	case "aws:kms":
		input.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		if s.cfg.KMSKeyID != "" {
			input.SSEKMSKeyId = aws.String(s.cfg.KMSKeyID)
		}
	}

	_, err = s.uploader.Upload(ctx, input)
	return err
}

