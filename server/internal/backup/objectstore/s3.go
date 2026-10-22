package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/time/rate"
)

type S3Config struct {
	Bucket          string
	Prefix          string
	Region          string
	Endpoint        string // for B2 / R2 / Wasabi / MinIO; empty for AWS
	AccessKeyID     string // empty = default AWS credential chain (env, ~/.aws)
	SecretAccessKey string
	PathStyle       bool // MinIO usually needs this
	BandwidthLimit  int64
}

type S3 struct {
	cfg      S3Config
	client   *s3.Client
	uploader *manager.Uploader
	limiter  *rate.Limiter
}

func OpenS3(ctx context.Context, c S3Config) (*S3, error) {
	if c.Bucket == "" {
		return nil, errors.New("s3: bucket is required")
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if c.Region != "" {
		opts = append(opts, awsconfig.WithRegion(c.Region))
	}
	if c.AccessKeyID != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, "")))
	}
	ac, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if ac.Region == "" {
		ac.Region = "us-east-1"
	}
	client := s3.NewFromConfig(ac, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.PathStyle
		// Third-party S3 implementations often don't support the newer
		// default checksum behaviour; we set checksums explicitly instead.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	return &S3{
		cfg:    c,
		client: client,
		uploader: manager.NewUploader(client, func(u *manager.Uploader) {
			u.PartSize = 16 << 20
			u.Concurrency = 2
		}),
		limiter: NewLimiter(c.BandwidthLimit),
	}, nil
}

func (s *S3) Describe() string {
	d := "s3://" + s.cfg.Bucket
	if s.cfg.Prefix != "" {
		d += "/" + strings.Trim(s.cfg.Prefix, "/")
	}
	if s.cfg.Endpoint != "" {
		d += " (" + s.cfg.Endpoint + ")"
	}
	return d
}

func (s *S3) key(k string) string {
	if p := strings.Trim(s.cfg.Prefix, "/"); p != "" {
		return p + "/" + k
	}
	return k
}

func (s *S3) unkey(k string) string {
	if p := strings.Trim(s.cfg.Prefix, "/"); p != "" {
		return strings.TrimPrefix(k, p+"/")
	}
	return k
}

func (s *S3) Put(ctx context.Context, key string, r io.ReadSeeker, size int64, opts PutOptions) error {
	class := types.StorageClass(opts.StorageClass)
	if class == "" {
		class = types.StorageClassStandard
	}
	body := throttle(ctx, r, s.limiter)
	if size > 64<<20 {
		_, err := s.uploader.Upload(ctx, &s3.PutObjectInput{
			Bucket: &s.cfg.Bucket, Key: aws.String(s.key(key)), Body: body,
			StorageClass: class, ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		})
		return wrapErr(err, key)
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.cfg.Bucket, Key: aws.String(s.key(key)), Body: body, ContentLength: aws.Int64(size),
		StorageClass: class, ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	})
	return wrapErr(err, key)
}

func (s *S3) Get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	in := &s3.GetObjectInput{Bucket: &s.cfg.Bucket, Key: aws.String(s.key(key))}
	if offset > 0 || length >= 0 {
		rng := fmt.Sprintf("bytes=%d-", offset)
		if length >= 0 {
			rng = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
		}
		in.Range = &rng
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		return nil, wrapErr(err, key)
	}
	return out.Body, nil
}

func (s *S3) Head(ctx context.Context, key string) (ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.cfg.Bucket, Key: aws.String(s.key(key))})
	if err != nil {
		return ObjectInfo{}, wrapErr(err, key)
	}
	info := ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), StorageClass: string(out.StorageClass)}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	info.Restore = restoreState(info.StorageClass, aws.ToString(out.Restore))
	return info, nil
}

func archivedClass(c string) bool {
	return c == string(types.StorageClassGlacier) || c == string(types.StorageClassDeepArchive)
}

// restoreState parses the x-amz-restore header, e.g.
// `ongoing-request="false", expiry-date="Fri, 21 Dec 2012 00:00:00 GMT"`.
func restoreState(class, header string) RestoreState {
	if !archivedClass(class) {
		return RestoreNotNeeded
	}
	switch {
	case header == "":
		return RestoreNone
	case strings.Contains(header, `ongoing-request="true"`):
		return RestoreInProgress
	default:
		return RestoreReady
	}
}

func (s *S3) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.cfg.Bucket, Prefix: aws.String(s.key(prefix))})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return wrapErr(err, prefix)
		}
		for _, o := range page.Contents {
			info := ObjectInfo{Key: s.unkey(aws.ToString(o.Key)), Size: aws.ToInt64(o.Size), StorageClass: string(o.StorageClass)}
			if o.LastModified != nil {
				info.LastModified = *o.LastModified
			}
			if !archivedClass(info.StorageClass) {
				info.Restore = RestoreNotNeeded
			}
			if err := fn(info); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *S3) Delete(ctx context.Context, keys ...string) error {
	for start := 0; start < len(keys); start += 1000 {
		batch := keys[start:min(start+1000, len(keys))]
		objs := make([]types.ObjectIdentifier, len(batch))
		for i, k := range batch {
			objs[i] = types.ObjectIdentifier{Key: aws.String(s.key(k))}
		}
		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &s.cfg.Bucket, Delete: &types.Delete{Objects: objs, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return wrapErr(err, "")
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("delete %s: %s", aws.ToString(e.Key), aws.ToString(e.Message))
		}
	}
	return nil
}

func (s *S3) RequestRestore(ctx context.Context, key, tier string, days int) error {
	_, err := s.client.RestoreObject(ctx, &s3.RestoreObjectInput{
		Bucket: &s.cfg.Bucket, Key: aws.String(s.key(key)),
		RestoreRequest: &types.RestoreRequest{
			Days:                 aws.Int32(int32(days)),
			GlacierJobParameters: &types.GlacierJobParameters{Tier: types.Tier(tier)},
		},
	})
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "RestoreAlreadyInProgress", "InvalidObjectState": // already restoring / not archived
			return nil
		}
	}
	return wrapErr(err, key)
}

func wrapErr(err error, key string) error {
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		switch ae.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return fmt.Errorf("%w: %s", ErrNotFound, key)
		case "InvalidObjectState":
			return fmt.Errorf("%w: %s", ErrArchived, key)
		}
	}
	return fmt.Errorf("s3 %s: %w", key, err)
}

// EnsureBucket checks the bucket exists and, if create is set, creates it.
func (s *S3) EnsureBucket(ctx context.Context, create bool) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.cfg.Bucket})
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if !errors.As(err, &ae) || (ae.ErrorCode() != "NotFound" && ae.ErrorCode() != "NoSuchBucket") {
		return wrapErr(err, s.cfg.Bucket)
	}
	if !create {
		return fmt.Errorf("bucket %s does not exist (create it, or pass --create-bucket)", s.cfg.Bucket)
	}
	in := &s3.CreateBucketInput{Bucket: &s.cfg.Bucket}
	if r := s.client.Options().Region; r != "" && r != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{LocationConstraint: types.BucketLocationConstraint(r)}
	}
	_, err = s.client.CreateBucket(ctx, in)
	return wrapErr(err, s.cfg.Bucket)
}
