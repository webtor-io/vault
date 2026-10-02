package services

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	awss3 "github.com/aws/aws-sdk-go/service/s3"
)

// Over TLS a part goes up without the SDK hashing its body; over plain
// HTTP the signed payload hash stays.
func TestUnsignedOverTLS(t *testing.T) {
	for _, c := range []struct {
		name     string
		srv      func(http.Handler) *httptest.Server
		unsigned bool
	}{
		{"https", httptest.NewTLSServer, true},
		{"http", httptest.NewServer, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got http.Header
			srv := c.srv(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Clone()
				w.Header().Set("ETag", `"etag"`)
			}))
			defer srv.Close()
			cl := awss3.New(session.Must(session.NewSession(&aws.Config{
				Credentials:      credentials.NewStaticCredentials("id", "secret", ""),
				Endpoint:         aws.String(srv.URL),
				Region:           aws.String("de"),
				S3ForcePathStyle: aws.Bool(true),
				HTTPClient:       srv.Client(),
			})))
			_, err := cl.UploadPartWithContext(context.Background(), &awss3.UploadPartInput{
				Bucket: aws.String("b"), Key: aws.String("k"), UploadId: aws.String("u"),
				PartNumber: aws.Int64(1), Body: bytes.NewReader([]byte("part body")),
			}, unsignedOverTLS)
			if err != nil {
				t.Fatal(err)
			}
			sha, md5 := got.Get("X-Amz-Content-Sha256"), got.Get("Content-Md5")
			if c.unsigned && (sha != "UNSIGNED-PAYLOAD" || md5 != "") {
				t.Errorf("over TLS: X-Amz-Content-Sha256=%q Content-Md5=%q, want UNSIGNED-PAYLOAD and none", sha, md5)
			}
			if !c.unsigned && (sha == "UNSIGNED-PAYLOAD" || len(sha) != 64) {
				t.Errorf("over plain HTTP: X-Amz-Content-Sha256=%q, want the body's SHA-256", sha)
			}
		})
	}
}
