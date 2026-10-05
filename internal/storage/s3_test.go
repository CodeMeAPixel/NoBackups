package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/codemeapixel/nobackups/internal/config"
)

type fakeAlarik struct {
	mu        sync.Mutex
	weakETags bool
	parts     map[int]string
	partData  map[int][]byte
	objects   map[string][]byte
	completes int
}

func newFakeAlarik(weak bool) *fakeAlarik {
	return &fakeAlarik{weakETags: weak, parts: map[int]string{}, partData: map[int][]byte{}, objects: map[string][]byte{}}
}

func (f *fakeAlarik) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	etagHeader := func(sum string) {
		if f.weakETags {
			w.Header().Set("ETag", `W/"`+sum+`"`)
		} else {
			w.Header().Set("ETag", `"`+sum+`"`)
		}
	}
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		sum := md5.Sum(body)
		f.parts[n] = hex.EncodeToString(sum[:])
		f.partData[n] = body
		etagHeader(hex.EncodeToString(sum[:]))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var req struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		xml.Unmarshal(body, &req)
		var all []byte
		for _, p := range req.Parts {
			if f.parts[p.PartNumber] != strings.ReplaceAll(strings.TrimSpace(p.ETag), `"`, "") {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `<Error><Code>InvalidPart</Code><Message>ETag mismatch for part %d</Message></Error>`, p.PartNumber)
				return
			}
			all = append(all, f.partData[p.PartNumber]...)
		}
		f.objects[r.URL.Path] = all
		f.completes++
		fmt.Fprint(w, `<CompleteMultipartUploadResult><Bucket>bkt</Bucket><Key>k</Key><ETag>"x-2"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[r.URL.Path] = body
		sum := md5.Sum(body)
		etagHeader(hex.EncodeToString(sum[:]))
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func testS3(t *testing.T, srv *httptest.Server) *S3 {
	t.Helper()
	tr := true
	s, err := NewS3(&config.Destination{
		Name: "alarik", Type: "s3", Endpoint: strings.TrimPrefix(srv.URL, "https://"), UseSSL: &tr, InsecureSkipVerify: true,
		Bucket: "bkt", AccessKeyID: "k", SecretAccessKey: "s", Region: "us-east-1", PathStyle: true, PartSizeMB: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUploadThroughETagWeakeningProxy(t *testing.T) {
	for _, weak := range []bool{false, true} {
		t.Run(fmt.Sprintf("weak=%v", weak), func(t *testing.T) {
			fake := newFakeAlarik(weak)
			srv := httptest.NewTLSServer(fake)
			defer srv.Close()
			s := testS3(t, srv)
			ctx := context.Background()

			data := bytes.Repeat([]byte("0123456789"), 1_200_000)
			if err := s.Put(ctx, "big", bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			if fake.completes != 1 || len(fake.parts) != 3 {
				t.Errorf("expected one 3-part upload, got %d completes, %d parts", fake.completes, len(fake.parts))
			}
			if got := fake.objects["/bkt/big"]; !bytes.Equal(got, data) {
				t.Errorf("stored %d bytes, want %d", len(got), len(data))
			}

			if err := s.Put(ctx, "small", strings.NewReader("tiny")); err != nil {
				t.Fatal(err)
			}
			if fake.completes != 1 || string(fake.objects["/bkt/small"]) != "tiny" {
				t.Error("small objects should use a single PUT")
			}

			if err := s.CheckMultipart(ctx, "check"); err != nil {
				t.Fatal(err)
			}
			if fake.completes != 2 {
				t.Error("CheckMultipart must exercise a multipart upload")
			}
		})
	}
}

func TestMinioUploaderFailsBehindETagWeakeningProxy(t *testing.T) {
	srv := httptest.NewTLSServer(newFakeAlarik(true))
	defer srv.Close()
	s := testS3(t, srv)
	_, err := s.client.PutObject(context.Background(), "bkt", "x", bytes.NewReader(make([]byte, 6<<20)), -1,
		minio.PutObjectOptions{PartSize: 5 << 20})
	if err == nil || !strings.Contains(err.Error(), "ETag mismatch for part 1") {
		t.Fatalf("expected minio-go's own uploader to hit the weak-ETag mismatch, got %v", err)
	}
}

func TestUploadExactPartMultiple(t *testing.T) {
	fake := newFakeAlarik(false)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	s := testS3(t, srv)
	data := make([]byte, 10<<20)
	if err := s.Put(context.Background(), "exact", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if len(fake.parts) != 2 || len(fake.objects["/bkt/exact"]) != len(data) {
		t.Errorf("got %d parts, %d bytes", len(fake.parts), len(fake.objects["/bkt/exact"]))
	}
}
