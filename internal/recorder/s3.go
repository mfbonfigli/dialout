package recorder

import (
	"context"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3 uploads to a bucket. The credentials need only s3:PutObject under calls/: no reads, no
// deletes. The bucket encrypts at rest by default.
type S3 struct {
	Client *s3.Client
	Bucket string
}

func (u *S3) Put(ctx context.Context, key, contentType string, body io.ReadSeeker) error {
	_, err := u.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(u.Bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	return err
}
