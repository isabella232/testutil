package s3test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/grailbio/base/cloud/url"
	"github.com/grailbio/testutil"
)

// awsContentSha256Key is the header used to store the sha256 of
// a file's content in the grail.com/pipeline.
const awsContentSha256Key = "Content-Sha256"

func sha256Digest(body []byte, meta map[string]*string) (string, error) {
	bodySum := fmt.Sprintf("%x", sha256.Sum256(body))
	if sumBytes, ok := meta[awsContentSha256Key]; ok {
		sum := aws.StringValue(sumBytes)
		if sum != bodySum {
			return "", fmt.Errorf("sha256 checksum mismatch: got %v, expect %v for %v",
				sum, bodySum, string(body))
		}
	}
	return bodySum, nil
}

type multipartUpload struct {
	id      string             // uploadID
	key     string             // s3 path
	etag    string             // etag to be assigned to the new file
	meta    map[string]*string // metadata sent in CreateMultiPartUpload request
	partial map[int64][]byte
}

// Client implements s3iface.S3API by using an AWS SDK client and
// overriding methods under test: HeadObject, ListObjectsV2,
// PutObjectRequest, CreateMultipartUploadRequest, UploadPartRequest,
// AbortMultipartUploadRequest, CompleteMultipartUploadRequest,
// GetObjectRequest, CopyObject, and DeleteObject. (These methods are
// sufficient to use with the S3 upload and download managers.)
//
// File contents (and their checksums) are provided by the user.
type Client struct {
	// Region holds the region of the bucket returned by
	// GetBucketLocationRequest.
	Region string

	// NumMaxRetries configures the maximum number of retries permitted
	// for operations involving this client.
	NumMaxRetries int

	// If Err!=nil, it is returned by any s3 method call.
	Err error

	s3iface.S3API
	svc      s3iface.S3API
	bucket   string
	m        sync.Mutex
	content  map[string]FileContent      // maps s3 key
	uploads  map[string]*multipartUpload // active multipart upload requests
	apiCount map[string]int              // maps the s3 api methods to occurence counts
	t        *testing.T

	seqMu sync.Mutex // For generating unique IDs.
	seq   int
}

func parseByteRange(s string, contentLen int64) (int64, int64, error) {
	prefix := "bytes="
	if !strings.HasPrefix(s, prefix) {
		return -1, -1, fmt.Errorf("parseByteRange %v: range must start with by bytes=", s)
	}
	s = strings.TrimPrefix(s, "bytes=")
	if strings.HasSuffix(s, "-") {
		// "start-"
		start, err := strconv.ParseInt(s[:len(s)-1], 10, 64)
		if err != nil {
			return 0, contentLen - 1, fmt.Errorf("parseByteRange %v: could not parse start", s)
		}
		return start, contentLen - 1, nil
	}
	if strings.HasPrefix(s, "-") {
		len, err := strconv.ParseInt(s[1:], 10, 64)
		if err != nil {
			return 0, contentLen - 1, fmt.Errorf("parseByteRange %v: could not parse suffix length", s)
		}
		return contentLen - len, contentLen - 1, nil
	}
	parts := strings.Split(s, "-")
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, contentLen - 1, fmt.Errorf("parseByteRange %v: could not parse start", s)
	}
	last, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, contentLen - 1, fmt.Errorf("parseByteRange %v: could not parse end", s)
	}
	return start, last, nil
}

// FileContent stores the file content and the metadata.
type FileContent struct {
	content      testutil.ContentAt
	sha256       string
	lastModified time.Time
	etag         string
}

func (c *Client) newETag() string {
	c.seqMu.Lock()
	s := fmt.Sprintf("testetag%d", c.seq)
	c.seq++
	c.seqMu.Unlock()
	return s
}

func (c *Client) newUploadID() string {
	c.seqMu.Lock()
	s := fmt.Sprintf("testuploadid%d", c.seq)
	c.seq++
	c.seqMu.Unlock()
	return s
}

// NewClient constructs a new S3 client under test. The client
// reports errors to the given testing.T, and expects to receive
// requests for the given bucket.
func NewClient(t *testing.T, bucket string) *Client {
	// There are different ways of handling the XXXRequest vs XXX API methods.
	// - The XXX methods directly return a result so that's easy,
	//   just return a custom result.
	// - The XXXRequest methods as used by s3manager, return a request
	//   that s3manager tweaks and then calls its Send() method.  Here
	//   we subcontract the building of the request out to the real S3API
	//   implementation, get rid of its Handlers, patch up the output, and
	//   then insert a noop Send Handler
	svc := s3.New(session.New(), nil)
	svc.Handlers.Clear()
	return &Client{
		svc:      svc,
		bucket:   bucket,
		content:  make(map[string]FileContent),
		uploads:  make(map[string]*multipartUpload),
		apiCount: make(map[string]int),
		t:        t,
	}
}

// MaxRetries returns the maximum number of retries permitted for operations
// using this client.
func (c *Client) MaxRetries() int {
	return c.NumMaxRetries
}

// GetFile returns the file contents and its metadata. Returns false if the file
// is not found.
func (c *Client) GetFile(key string) (FileContent, bool) {
	c.m.Lock()
	defer c.m.Unlock()
	f, ok := c.content[key]
	return f, ok
}

// SetFileContent defines the body for key.
func (c *Client) SetFile(key string, content []byte, sha256 string) {
	c.SetFileContentAt(key, &testutil.ByteContent{content}, sha256)
}

// SetFileContentReader sets the underlying TestReader for content.
func (c *Client) SetFileContentAt(key string, content testutil.ContentAt, sha256 string) {
	c.m.Lock()
	defer c.m.Unlock()
	c.content[key] = FileContent{
		content:      content,
		sha256:       sha256,
		lastModified: time.Now(),
		etag:         c.newETag(),
	}
}

// GetFileContentBytes returns the byte slice representation of the contents for key.
func (c *Client) GetFileContentBytes(key string) []byte {
	c.m.Lock()
	defer c.m.Unlock()
	result := make([]byte, c.content[key].content.Size())
	c.content[key].content.ReadAt(result, 0)
	return result
}

// setFileFromPartialContent collects the content from partial and sets key in content with the result.
func (c *Client) setFileFromPartialContent(key string, uploadID string, parts []*s3.CompletedPart) {
	c.m.Lock()
	defer c.m.Unlock()

	r := c.uploads[uploadID]
	if r == nil {
		c.t.Errorf("UploadPartRequest: unknown upload ID %s", uploadID)
		return
	}
	if r.key != key {
		c.t.Errorf("Key mismatch: %v %v", r.key, key)
		return
	}
	if len(parts) != len(r.partial) {
		c.t.Errorf("Parts mismatch: %v %v", parts, r.partial)
		return
	}
	size := 0
	for _, b := range r.partial {
		size += len(b)
	}
	buf := make([]byte, size)
	pos := 0
	lastPartNum := int64(-1)
	for _, part := range parts {
		if *part.PartNumber <= lastPartNum {
			c.t.Errorf("Unsorted part number %d %d", *part.PartNumber, lastPartNum)
			return
		}
		lastPartNum = *part.PartNumber
		bb, ok := r.partial[*part.PartNumber]
		if !ok {
			c.t.Errorf("Missing part %d", *part.PartNumber)
		} else {
			copy(buf[pos:], bb)
			pos += len(bb)
			delete(r.partial, *part.PartNumber)
		}
	}
	sha, err := sha256Digest(buf, r.meta)
	if err != nil {
		panic(err)
	}
	c.content[key] = FileContent{
		content:      &testutil.ByteContent{buf},
		sha256:       sha,
		lastModified: time.Now(),
		etag:         r.etag,
	}
	delete(c.uploads, uploadID)
}

func (c *Client) copyFile(src, dst string, meta map[string]*string) error {
	c.m.Lock()
	defer c.m.Unlock()
	if sha256, ok := meta[awsContentSha256Key]; ok {
		if sum := aws.StringValue(sha256); sum != c.content[src].sha256 {
			return fmt.Errorf("copyfile %s->%s: sha256 checksum mismatch: %s <-> %s", src, dst, sum, c.content[src].sha256)
		}
	}
	c.content[dst] = c.content[src]
	return nil
}

func (c *Client) deleteFile(key string) {
	c.m.Lock()
	defer c.m.Unlock()
	delete(c.content, key)
}

func (c *Client) incApiCount(api string) {
	c.m.Lock()
	defer c.m.Unlock()
	c.apiCount[api]++
}

// GetApiCount returns the number of invocations for the given API
// GetApiCount returns call. counts only for methods that are under
// GetApiCount returns test.
func (c *Client) GetApiCount(api string) int {
	c.m.Lock()
	defer c.m.Unlock()
	return c.apiCount[api]
}

// HeadObject is used in s3-loader to determine if an object in S3 and
// the local matching object are identical.
func (c *Client) HeadObject(
	input *s3.HeadObjectInput) (output *s3.HeadObjectOutput, err error) {
	c.incApiCount("HeadObject")
	if c.Err != nil {
		return nil, c.Err
	}
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("HeadObject received unexpected bucket got: %s want %s", got, want)
	}

	key := aws.StringValue(input.Key)
	f, ok := c.GetFile(key)
	if !ok {
		return nil, awserr.New("NoSuchKey", "Object not found", nil)
	}
	output = &s3.HeadObjectOutput{
		ContentLength: aws.Int64(f.content.Size()),
		LastModified:  aws.Time(f.lastModified),
		ETag:          aws.String(f.etag),
		Metadata: map[string]*string{
			awsContentSha256Key: aws.String(f.sha256),
		},
	}
	return output, nil
}

func (c *Client) maybeOverrideError(r *request.Request) {
	if c.Err != nil && r.Error == nil {
		r.Error = c.Err
	}
}

// HeadObjectWithContext is the same as HeadObject, but allows passing a
// context and options.
func (c *Client) HeadObjectWithContext(
	ctx aws.Context, input *s3.HeadObjectInput, opts ...request.Option) (output *s3.HeadObjectOutput, err error) {
	c.incApiCount("HeadObjectRequestWithContext")
	req, out := c.HeadObjectRequest(input)
	defer c.maybeOverrideError(req)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// HeadObjectRequest creates an RPC request for HeadObject.
func (c *Client) HeadObjectRequest(input *s3.HeadObjectInput) (req *request.Request, out *s3.HeadObjectOutput) {
	c.incApiCount("HeadObjectRequest")
	var err error
	req, out = c.svc.HeadObjectRequest(input)
	defer c.maybeOverrideError(req)
	out1, err := c.HeadObject(input)
	if err != nil {
		req.Error = err
	} else {
		*out = *out1
	}
	req.Handlers.Send.Clear()
	req.Handlers.Clear()
	return
}

// ListObjectsV2WithContext is used by DownloadDirTree to detemine all the files
// to download.
func (c *Client) ListObjectsV2WithContext(
	ctx aws.Context, input *s3.ListObjectsV2Input, opts ...request.Option) (*s3.ListObjectsV2Output, error) {
	c.incApiCount("ListObjectsV2WithContext")
	req, out := c.ListObjectsV2Request(input)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// ListObjectsV2 is used by DownloadDirTree to detemine all the files
// to download.
func (c *Client) ListObjectsV2(input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
	c.incApiCount("ListObjectsV2")
	if c.Err != nil {
		return nil, c.Err
	}
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("ListObjectsV2 received unexpected bucket got: %s want %s", got, want)
	}
	prefix := aws.StringValue(input.Prefix)
	output := &s3.ListObjectsV2Output{
		IsTruncated: aws.Bool(false),
	}

	c.m.Lock()
	defer c.m.Unlock()
	for key, content := range c.content {
		if strings.HasPrefix(key, prefix) {
			object := s3.Object{
				Key:          aws.String(key),
				Size:         aws.Int64(content.content.Size()),
				LastModified: aws.Time(content.lastModified),
				ETag:         aws.String(content.etag),
			}
			output.Contents = append(output.Contents, &object)
		}
	}
	return output, nil
}

// ListObjectsV2Request implements the request variant of ListObjectsV2.
func (c *Client) ListObjectsV2Request(
	input *s3.ListObjectsV2Input) (req *request.Request, output *s3.ListObjectsV2Output) {
	c.incApiCount("ListObjectsV2Request")
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("ListObjectsV2 received unexpected bucket got: %s want %s", got, want)
	}
	req, output = c.svc.ListObjectsV2Request(input)
	defer c.maybeOverrideError(req)
	outputp, err := c.ListObjectsV2(input)
	if err != nil {
		req.Error = err
	} else {
		*output = *outputp
	}
	return
}

// PutObjectRequest is used within s3manager to upload single part files.
func (c *Client) PutObjectRequest(
	input *s3.PutObjectInput) (req *request.Request, output *s3.PutObjectOutput) {
	c.incApiCount("PutObjectRequest")
	// c.t.Logf("PutObjectRequest input: %v", input)
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("PutObjectRequest received unexpected bucket got: %s want %s", got, want)
	}
	req, output = c.svc.PutObjectRequest(input)
	defer c.maybeOverrideError(req)

	key := aws.StringValue(input.Key)
	body, err := ioutil.ReadAll(input.Body)
	if err != nil {
		c.t.Errorf("PutObjectRequest when reading input.Body: %s", err)
	}
	sha256, err := sha256Digest(body, input.Metadata)
	if err != nil {
		c.t.Errorf("PutObjectRequest: checksum: %s", err)
	}
	c.SetFile(key, body, sha256)
	return
}

// PutObjectWithContext implements the corresponding s3iface.API method.
func (c *Client) PutObjectWithContext(ctx aws.Context, input *s3.PutObjectInput, opts ...request.Option) (*s3.PutObjectOutput, error) {
	req, out := c.PutObjectRequest(input)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// CreateMultipartUploadWithContext stubs the corresponding s3iface.API method.
func (c *Client) CreateMultipartUploadWithContext(
	ctx aws.Context, input *s3.CreateMultipartUploadInput, opts ...request.Option) (
	*s3.CreateMultipartUploadOutput, error) {
	c.incApiCount("CreateMultipartUploadWithContext")
	req, out := c.CreateMultipartUploadRequest(input)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// UploadPartWithContext stubs the corresponding s3iface.API method.
func (c *Client) UploadPartWithContext(
	ctx aws.Context, input *s3.UploadPartInput, opts ...request.Option) (
	*s3.UploadPartOutput, error) {
	name := "UploadPartWithContext"
	c.incApiCount(name)
	req, out := c.UploadPartRequest(input)
	req.Handlers.Unmarshal.Clear()
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// UploadPartCopyWithContext stubs the corresponding s3iface.API method.
func (c *Client) UploadPartCopyWithContext(
	ctx aws.Context, input *s3.UploadPartCopyInput, opts ...request.Option) (
	*s3.UploadPartCopyOutput, error) {
	c.incApiCount("UploadPartCopyWithContext")
	req, out := c.UploadPartCopyRequest(input)
	req.Handlers.Unmarshal.Clear()
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// CompleteMultipartUploadWithContext stubs the corresponding s3iface.API method.
func (c *Client) CompleteMultipartUploadWithContext(
	ctx aws.Context, input *s3.CompleteMultipartUploadInput, opts ...request.Option) (
	*s3.CompleteMultipartUploadOutput, error) {
	c.incApiCount("CompleteMultipartUploadWithContext")
	req, out := c.CompleteMultipartUploadRequest(input)
	req.Handlers.Unmarshal.Clear()
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// CreateMultipartUploadRequest stubs the corresponding s3iface.API method.
func (c *Client) CreateMultipartUploadRequest(
	input *s3.CreateMultipartUploadInput) (req *request.Request, output *s3.CreateMultipartUploadOutput) {
	c.incApiCount("CreateMultipartUploadRequest")
	req, output = c.svc.CreateMultipartUploadRequest(input)
	defer c.maybeOverrideError(req)
	uploadID := c.newUploadID()
	r := &multipartUpload{
		id:      uploadID,
		key:     aws.StringValue(input.Key),
		etag:    "testetag:" + uploadID,
		meta:    input.Metadata,
		partial: map[int64][]byte{},
	}
	output.SetUploadId(r.id)
	c.m.Lock()
	defer c.m.Unlock()
	c.uploads[r.id] = r
	return req, output
}

// UploadPartRequest stubs the corresponding s3iface.API method.
func (c *Client) UploadPartRequest(
	input *s3.UploadPartInput) (req *request.Request, output *s3.UploadPartOutput) {
	c.incApiCount("UploadPartRequest")
	req, output = c.svc.UploadPartRequest(input)
	defer c.maybeOverrideError(req)
	uploadID := aws.StringValue(input.UploadId)
	body, err := ioutil.ReadAll(input.Body)
	if err != nil {
		c.t.Errorf("UploadPartRequest when reading input.Body: %s", err)
		return
	}
	c.m.Lock()
	defer c.m.Unlock()
	r := c.uploads[uploadID]
	if r == nil {
		c.t.Errorf("UploadPartRequest: unknown upload ID %s", uploadID)
		return
	}
	r.partial[aws.Int64Value(input.PartNumber)] = body
	output.SetETag(r.etag)
	return req, output
}

// UploadPartCopyRequest stubs the corresponding s3iface.API method.
func (c *Client) UploadPartCopyRequest(
	input *s3.UploadPartCopyInput) (req *request.Request, output *s3.UploadPartCopyOutput) {
	c.incApiCount("UploadPartCopyRequest")
	req, output = c.svc.UploadPartCopyRequest(input)
	defer c.maybeOverrideError(req)
	uploadID := aws.StringValue(input.UploadId)
	source, err := url.Decode(aws.StringValue(input.CopySource))
	if err != nil {
		c.t.Errorf("UploadPartCopyRequest could not unescape CopySource: %s", aws.StringValue(input.CopySource))
	}
	if !strings.HasPrefix(source, c.bucket+"/") {
		c.t.Errorf("UploadPartCopyRequest expected copy source from the same bucket, got: %v", source)
	}
	src := strings.TrimPrefix(source, c.bucket+"/")
	b, ok := c.GetFile(src)
	if !ok {
		c.t.Errorf("UploadPartCopyRequest source %f does not exist", src)
	}
	start := int64(0)
	last := b.content.Size() - 1
	if input.CopySourceRange != nil {
		var err error
		start, last, err = parseByteRange(aws.StringValue(input.CopySourceRange), b.content.Size())
		if err != nil {
			c.t.Errorf("UploadPartCopyRequest: %v", err)
		}
	}

	data := make([]byte, last-start+1)
	if _, err := b.content.ReadAt(data, start); err != nil {
		c.t.Fatal(err)
	}

	c.m.Lock()
	defer c.m.Lock()
	r := c.uploads[uploadID]
	if r == nil {
		c.t.Errorf("UploadPartRequest: unknown upload ID %s", uploadID)
		return
	}
	r.partial[aws.Int64Value(input.PartNumber)] = data
	output.SetCopyPartResult(&s3.CopyPartResult{
		ETag: aws.String("etag"),
	})
	return req, output
}

// AbortMultipartUploadRequest stubs the corresponding s3iface.API method.
func (c *Client) AbortMultipartUploadRequest(
	input *s3.AbortMultipartUploadInput) (req *request.Request, output *s3.AbortMultipartUploadOutput) {
	c.incApiCount("AbortMultipartUploadRequest")
	req, output = c.svc.AbortMultipartUploadRequest(input)
	defer c.maybeOverrideError(req)
	uploadID := aws.StringValue(input.UploadId)
	c.m.Lock()
	delete(c.uploads, uploadID)
	c.m.Unlock()
	return req, output
}

// AbortMultipartUploadWithContext implements the corresponding s3iface.API method.
func (c *Client) AbortMultipartUploadWithContext(
	ctx aws.Context, input *s3.AbortMultipartUploadInput,
	opts ...request.Option) (*s3.AbortMultipartUploadOutput, error) {
	req, out := c.AbortMultipartUploadRequest(input)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// CompleteMultipartUploadRequest stubs the corresponding s3iface.API method.
func (c *Client) CompleteMultipartUploadRequest(
	input *s3.CompleteMultipartUploadInput) (req *request.Request, output *s3.CompleteMultipartUploadOutput) {
	c.incApiCount("CompleteMultipartUploadRequest")
	req, output = c.svc.CompleteMultipartUploadRequest(input)
	defer c.maybeOverrideError(req)
	uploadID := aws.StringValue(input.UploadId)
	key := aws.StringValue(input.Key)
	c.setFileFromPartialContent(key, uploadID, input.MultipartUpload.Parts)
	return req, output
}

// GetObjectRequest is used by by s3manager (aws-sdk < 1.8.0) to downoad files.
// GetObjectRequest is used by GetObjectWithContext by s3manager (aws-sdk >= 1.8.0) to downoad files.
func (c *Client) GetObjectRequest(
	input *s3.GetObjectInput) (req *request.Request, output *s3.GetObjectOutput) {
	c.incApiCount("GetObjectRequest")
	// c.t.Logf("GetObjectRequest input: %v", input)
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("GetObjectRequest received unexpected bucket got: %s want %s", got, want)
	}
	req, output = c.svc.GetObjectRequest(input)
	defer c.maybeOverrideError(req)
	key := aws.StringValue(input.Key)
	b, ok := c.GetFile(key)
	if !ok {
		c.t.Logf("GetObjectRequest no file content for: %s", key)
		output.Body = ioutil.NopCloser(bytes.NewReader(make([]byte, 0)))
		output.ContentLength = aws.Int64(0)
		output.LastModified = aws.Time(time.Time{})
		output.ETag = aws.String("")
	} else {
		start := int64(0)
		last := b.content.Size() - 1
		if input.Range != nil {
			var err error
			start, last, err = parseByteRange(aws.StringValue(input.Range), b.content.Size())
			if err != nil {
				c.t.Errorf("GetObjectRequest: %v", err)
			}
		}
		if (last + 1) >= b.content.Size() {
			output.Body = ioutil.NopCloser(io.NewSectionReader(b.content, start, b.content.Size()-start))
			if start > 0 {
				last = b.content.Size() - 1
				output.ContentRange = aws.String(fmt.Sprintf("bytes %d-%d/%d", start, last, b.content.Size()))
			}
			output.ContentLength = aws.Int64(b.content.Size() - start)
		} else {
			output.Body = ioutil.NopCloser(io.NewSectionReader(b.content, start, last-start+1))
			output.ContentRange = aws.String(fmt.Sprintf("bytes %d-%d/%d", start, last, b.content.Size()))
			output.ContentLength = aws.Int64(last - start + 1)
		}
		output.LastModified = aws.Time(b.lastModified)
		output.ETag = aws.String(b.etag)
	}
	// c.t.Logf("GetObjectRequest output: %v", output)
	req.Handlers.Send.PushBack(func(r *request.Request) {
		// c.t.Logf("get params: %T\n", r.Params)
	})
	return
}

// CopyObjectRequest implements the Request model of server side object copying.
func (c *Client) CopyObjectRequest(
	input *s3.CopyObjectInput) (req *request.Request, output *s3.CopyObjectOutput) {
	c.incApiCount("CopyObjectRequest")
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("CopyObject received unexpected bucket got: %s want %s", got, want)
	}
	req, output = c.svc.CopyObjectRequest(input)
	req.Handlers.Unmarshal.Clear()

	// c.t.Logf("CopyObjectRequest input: %v", *input)
	source, err := url.Decode(aws.StringValue(input.CopySource))
	if err != nil {
		c.t.Errorf("CopyObjectRequest could not unescape CopySource: %s", aws.StringValue(input.CopySource))
	}
	if !strings.HasPrefix(source, c.bucket+"/") {
		c.t.Errorf("CopyObject expected copy source from the same bucket, got: %v", source)
	}
	src, dst := strings.TrimPrefix(source, c.bucket+"/"), aws.StringValue(input.Key)
	if err := c.copyFile(src, dst, input.Metadata); err != nil {
		c.t.Errorf("CopyObjectRequest: %v", err)
	}
	return
}

// CopyObject implements S3-side object copying.
func (c *Client) CopyObject(input *s3.CopyObjectInput) (*s3.CopyObjectOutput, error) {
	c.incApiCount("CopyObject")
	if c.Err != nil {
		return nil, c.Err
	}
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("CopyObject received unexpected bucket got: %s want %s", got, want)
	}
	// c.t.Logf("CopyObject input: %v", *input)
	source, err := url.Decode(aws.StringValue(input.CopySource))
	if err != nil {
		c.t.Errorf("UploadPartCopyRequest could not unescape CopySource: %s", aws.StringValue(input.CopySource))
	}
	if !strings.HasPrefix(source, c.bucket+"/") {
		c.t.Errorf("CopyObject expected copy source from the same bucket, got: %v", source)
	}
	src, dst := strings.TrimPrefix(source, c.bucket+"/"), aws.StringValue(input.Key)
	if err := c.copyFile(src, dst, input.Metadata); err != nil {
		c.t.Errorf("CopyObjectRequest: %v", err)
	}
	return &s3.CopyObjectOutput{}, nil
}

// DeleteObject removes an object from the bucket.
func (c *Client) DeleteObject(input *s3.DeleteObjectInput) (*s3.DeleteObjectOutput, error) {
	c.incApiCount("DeleteObject")
	if c.Err != nil {
		return nil, c.Err
	}
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("DeleteObject received unexpected bucket got: %s want %s", got, want)
	}
	key := aws.StringValue(input.Key)
	c.deleteFile(key)
	return &s3.DeleteObjectOutput{}, nil
}

// DeleteObjectWithContext is the same as DeleteObject, but allows passing a
// context and options.
func (c *Client) DeleteObjectWithContext(ctx aws.Context, input *s3.DeleteObjectInput, opts ...request.Option) (*s3.DeleteObjectOutput, error) {
	c.incApiCount("DeleteObjectWithContext")
	req, out := c.DeleteObjectRequest(input)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// DeleteObjectRequest creates an RPC request for DeleteObject.
func (c *Client) DeleteObjectRequest(input *s3.DeleteObjectInput) (req *request.Request, out *s3.DeleteObjectOutput) {
	c.incApiCount("DeleteObjectRequest")
	var err error
	req, out = c.svc.DeleteObjectRequest(input)
	defer c.maybeOverrideError(req)
	out1, err := c.DeleteObject(input)
	if err != nil {
		req.Error = err
	} else {
		*out = *out1
	}
	req.Handlers.Send.Clear()
	req.Handlers.Clear()
	return
}

// GetObject retrieves an object from the bucket.
func (c *Client) GetObject(input *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	c.incApiCount("GetObject")
	if c.Err != nil {
		return nil, c.Err
	}
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("GetObject received unexpected bucket got: %s want %s", got, want)
	}

	output := s3.GetObjectOutput{}
	key := aws.StringValue(input.Key)
	b, ok := c.GetFile(key)
	if !ok {
		c.t.Logf("GetObject no file content for: %s", key)
		output.Body = ioutil.NopCloser(bytes.NewReader(make([]byte, 0)))
		output.ContentLength = aws.Int64(0)
		output.LastModified = aws.Time(time.Time{})
		output.ETag = aws.String("")
	} else {
		output.Body = ioutil.NopCloser(io.NewSectionReader(b.content, 0, b.content.Size()))
		output.ContentLength = aws.Int64(b.content.Size())
		output.LastModified = aws.Time(b.lastModified)
		output.ETag = aws.String(b.etag)
	}
	return &output, nil
}

// GetObjectWithContext is used within s3manager (aws-sdk >= 1.8.0) to downoad files,
// we leverage GetObjectRequest (from above) internally to do the work.
func (c *Client) GetObjectWithContext(
	ctx aws.Context, input *s3.GetObjectInput, opts ...request.Option) (*s3.GetObjectOutput, error) {
	c.incApiCount("GetObjectWithContext")

	// This implementation taken from svc.GetObjectWithContext()
	req, out := c.GetObjectRequest(input)
	req.SetContext(ctx)
	req.ApplyOptions(opts...)
	return out, req.Send()
}

// GetBucketLocationRequest implements the bucket location (Client.Region)
// request.
func (c *Client) GetBucketLocationRequest(input *s3.GetBucketLocationInput) (req *request.Request, output *s3.GetBucketLocationOutput) {
	c.incApiCount("GetBucketLocationRequest")
	if got, want := aws.StringValue(input.Bucket), c.bucket; got != want {
		c.t.Errorf("GetBucketLocationRequest received unexpected bucket got: %s want %s", got, want)
	}
	req, output = c.svc.GetBucketLocationRequest(input)
	defer c.maybeOverrideError(req)
	output.SetLocationConstraint(c.Region)
	req.Handlers.Send.Clear()
	req.Handlers.Clear()
	return
}

// PutObjectAcl sets the ACL of an object already in the bucket.
func (c *Client) PutObjectAcl(input *s3.PutObjectAclInput) (*s3.PutObjectAclOutput, error) {
	c.incApiCount("PutObjectAcl")
	if c.Err != nil {
		return nil, c.Err
	}
	output := s3.PutObjectAclOutput{}
	return &output, nil
}
