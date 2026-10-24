// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package s3fake is a minimal in-memory S3 server for tests. It implements
// the path-style subset of the S3 API that the backup destinations use:
// single and multipart uploads, downloads, listing, deletion, and the
// listing and abort of incomplete multipart uploads. Requests are not
// authenticated.
package s3fake

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type object struct {
	data     []byte
	etag     string
	modified time.Time
	ctype    string
}

type upload struct {
	id        string
	bucket    string
	key       string
	initiated time.Time
	parts     map[int][]byte
}

// Server is an in-memory S3 server.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	buckets  map[string]map[string]*object
	uploads  map[string]*upload
	nextID   int
	failing  bool
	requests int
}

// New starts a fake S3 server. Buckets are created on first use.
func New() *Server {
	s := &Server{
		buckets: make(map[string]map[string]*object),
		uploads: make(map[string]*upload),
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Endpoint returns the http:// endpoint of the server.
func (s *Server) Endpoint() string {
	return s.URL
}

// SetFailing makes every request fail with AccessDenied, which S3 clients
// don't retry, to simulate an unavailable destination.
func (s *Server) SetFailing(failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = failing
}

// Keys returns the sorted keys of the objects of a bucket.
func (s *Server) Keys(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keys []string
	for k := range s.buckets[bucket] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Object returns the content of an object, or nil.
func (s *Server) Object(bucket, key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.buckets[bucket][key]; o != nil {
		return bytes.Clone(o.data)
	}
	return nil
}

// PutObject stores an object directly.
func (s *Server) PutObject(bucket, key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bucket(bucket)[key] = newObject(data, "")
}

// IncompleteUploads returns the number of incomplete multipart uploads.
func (s *Server) IncompleteUploads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.uploads)
}

// StartUpload starts a multipart upload directly, as an interrupted backup
// would leave behind.
func (s *Server) StartUpload(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("upload-%d", s.nextID)
	s.uploads[id] = &upload{id: id, bucket: bucket, key: key, initiated: time.Now(), parts: map[int][]byte{}}
}

func (s *Server) bucket(name string) map[string]*object {
	b, ok := s.buckets[name]
	if !ok {
		b = make(map[string]*object)
		s.buckets[name] = b
	}
	return b
}

func newObject(data []byte, ctype string) *object {
	sum := md5.Sum(data)
	return &object{data: data, etag: `"` + hex.EncodeToString(sum[:]) + `"`, modified: time.Now().UTC(), ctype: ctype}
}

type s3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Key       string   `xml:"Key,omitempty"`
	Bucket    string   `xml:"BucketName,omitempty"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, bucket, key string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_ = xml.NewEncoder(w).Encode(&s3Error{
		Code: code, Message: code, Key: key, Bucket: bucket, Resource: r.URL.Path, RequestID: "fake",
	})
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

// readBody returns the request body, decoding the aws-chunked encoding of
// streaming signatures.
func readBody(r *http.Request) ([]byte, error) {
	sha := r.Header.Get("X-Amz-Content-Sha256")
	if !strings.HasPrefix(sha, "STREAMING-") && !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
		return io.ReadAll(r.Body)
	}
	var out bytes.Buffer
	br := bufio.NewReader(r.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("invalid chunk header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		sizeHex, _, _ := strings.Cut(line, ";")
		size, err := strconv.ParseInt(sizeHex, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid chunk size %q", sizeHex)
		}
		if size == 0 {
			// Trailers, if any, follow; they are ignored.
			_, _ = io.Copy(io.Discard, br)
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, err
		}
		if _, err := br.Discard(2); err != nil {
			return nil, err
		}
	}
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests++
	failing := s.failing
	s.mu.Unlock()

	p := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(p, "/")
	if failing {
		_, _ = io.Copy(io.Discard, r.Body)
		writeError(w, r, http.StatusForbidden, "AccessDenied", bucket, key)
		return
	}
	if bucket == "" {
		writeError(w, r, http.StatusBadRequest, "InvalidRequest", "", "")
		return
	}
	q := r.URL.Query()

	if key == "" {
		s.handleBucket(w, r, bucket, q)
		return
	}

	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.initiateUpload(w, bucket, key)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		s.uploadPart(w, r, bucket, key, q)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		s.completeUpload(w, r, bucket, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		s.mu.Lock()
		delete(s.uploads, q.Get("uploadId"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && q.Has("uploadId"):
		s.listParts(w, r, bucket, key, q.Get("uploadId"))
	case r.Method == http.MethodPut:
		data, err := readBody(r)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "IncompleteBody", bucket, key)
			return
		}
		obj := newObject(data, r.Header.Get("Content-Type"))
		s.mu.Lock()
		s.bucket(bucket)[key] = obj
		s.mu.Unlock()
		w.Header().Set("ETag", obj.etag)
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		s.getObject(w, r, bucket, key)
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		delete(s.bucket(bucket), key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, r, http.StatusNotImplemented, "NotImplemented", bucket, key)
	}
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	s.mu.Lock()
	obj := s.bucket(bucket)[key]
	s.mu.Unlock()
	if obj == nil {
		writeError(w, r, http.StatusNotFound, "NoSuchKey", bucket, key)
		return
	}
	data := obj.data
	status := http.StatusOK
	if rng := r.Header.Get("Range"); rng != "" && strings.HasPrefix(rng, "bytes=") {
		startStr, endStr, _ := strings.Cut(strings.TrimPrefix(rng, "bytes="), "-")
		start, _ := strconv.ParseInt(startStr, 10, 64)
		end := int64(len(data)) - 1
		if endStr != "" {
			end, _ = strconv.ParseInt(endStr, 10, 64)
		}
		if startStr == "" { // suffix range
			n, _ := strconv.ParseInt(endStr, 10, 64)
			start, end = int64(len(data))-n, int64(len(data))-1
		}
		if end >= int64(len(data)) {
			end = int64(len(data)) - 1
		}
		if start < 0 || start > end {
			writeError(w, r, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", bucket, key)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		data = data[start : end+1]
		status = http.StatusPartialContent
	}
	ctype := obj.ctype
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("ETag", obj.etag)
	w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(status)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

func (s *Server) initiateUpload(w http.ResponseWriter, bucket, key string) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("upload-%d", s.nextID)
	s.uploads[id] = &upload{id: id, bucket: bucket, key: key, initiated: time.Now().UTC(), parts: map[int][]byte{}}
	s.mu.Unlock()
	writeXML(w, &struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}{Bucket: bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string, q map[string][]string) {
	id := first(q["uploadId"])
	n, err := strconv.Atoi(first(q["partNumber"]))
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "InvalidArgument", bucket, key)
		return
	}
	data, err := readBody(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "IncompleteBody", bucket, key)
		return
	}
	s.mu.Lock()
	u := s.uploads[id]
	if u != nil {
		u.parts[n] = data
	}
	s.mu.Unlock()
	if u == nil {
		writeError(w, r, http.StatusNotFound, "NoSuchUpload", bucket, key)
		return
	}
	sum := md5.Sum(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(http.StatusOK)
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func (s *Server) completeUpload(w http.ResponseWriter, r *http.Request, bucket, key, id string) {
	var req struct {
		Parts []struct {
			PartNumber int `xml:"PartNumber"`
		} `xml:"Part"`
	}
	body, err := readBody(r)
	if err == nil {
		err = xml.Unmarshal(body, &req)
	}
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "MalformedXML", bucket, key)
		return
	}

	s.mu.Lock()
	u := s.uploads[id]
	if u == nil {
		s.mu.Unlock()
		writeError(w, r, http.StatusNotFound, "NoSuchUpload", bucket, key)
		return
	}
	var data bytes.Buffer
	for _, p := range req.Parts {
		part, ok := u.parts[p.PartNumber]
		if !ok {
			s.mu.Unlock()
			writeError(w, r, http.StatusBadRequest, "InvalidPart", bucket, key)
			return
		}
		data.Write(part)
	}
	obj := newObject(data.Bytes(), "")
	s.bucket(bucket)[key] = obj
	delete(s.uploads, id)
	s.mu.Unlock()

	writeXML(w, &struct {
		XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
		Location string   `xml:"Location"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		ETag     string   `xml:"ETag"`
	}{Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: obj.etag})
}

func (s *Server) listParts(w http.ResponseWriter, r *http.Request, bucket, key, id string) {
	type part struct {
		PartNumber   int    `xml:"PartNumber"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	}
	s.mu.Lock()
	u := s.uploads[id]
	var parts []part
	if u != nil {
		for n, data := range u.parts {
			sum := md5.Sum(data)
			parts = append(parts, part{PartNumber: n, ETag: `"` + hex.EncodeToString(sum[:]) + `"`,
				Size: int64(len(data)), LastModified: u.initiated.Format(time.RFC3339)})
		}
	}
	s.mu.Unlock()
	if u == nil {
		writeError(w, r, http.StatusNotFound, "NoSuchUpload", bucket, key)
		return
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	writeXML(w, &struct {
		XMLName     xml.Name `xml:"ListPartsResult"`
		Bucket      string   `xml:"Bucket"`
		Key         string   `xml:"Key"`
		UploadID    string   `xml:"UploadId"`
		IsTruncated bool     `xml:"IsTruncated"`
		Parts       []part   `xml:"Part"`
	}{Bucket: bucket, Key: key, UploadID: id, Parts: parts})
}

func (s *Server) handleBucket(w http.ResponseWriter, r *http.Request, bucket string, q map[string][]string) {
	switch {
	case r.Method == http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && hasKey(q, "location"):
		writeXML(w, &struct {
			XMLName xml.Name `xml:"LocationConstraint"`
			Value   string   `xml:",chardata"`
		}{})
	case r.Method == http.MethodGet && hasKey(q, "uploads"):
		s.listUploads(w, bucket, first(q["prefix"]))
	case r.Method == http.MethodGet:
		s.listObjects(w, bucket, first(q["prefix"]), first(q["delimiter"]))
	case r.Method == http.MethodPost && hasKey(q, "delete"):
		s.deleteObjects(w, r, bucket)
	case r.Method == http.MethodPut:
		s.mu.Lock()
		s.bucket(bucket)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		writeError(w, r, http.StatusNotImplemented, "NotImplemented", bucket, "")
	}
}

func hasKey(q map[string][]string, k string) bool {
	_, ok := q[k]
	return ok
}

func (s *Server) listObjects(w http.ResponseWriter, bucket, prefix, delimiter string) {
	type content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	type commonPrefix struct {
		Prefix string `xml:"Prefix"`
	}
	s.mu.Lock()
	var contents []content
	prefixes := map[string]struct{}{}
	for k, o := range s.bucket(bucket) {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				prefixes[k[:len(prefix)+i+len(delimiter)]] = struct{}{}
				continue
			}
		}
		contents = append(contents, content{Key: k, LastModified: o.modified.Format(time.RFC3339),
			ETag: o.etag, Size: int64(len(o.data)), StorageClass: "STANDARD"})
	}
	s.mu.Unlock()
	sort.Slice(contents, func(i, j int) bool { return contents[i].Key < contents[j].Key })
	var cps []commonPrefix
	for p := range prefixes {
		cps = append(cps, commonPrefix{Prefix: p})
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i].Prefix < cps[j].Prefix })

	writeXML(w, &struct {
		XMLName        xml.Name       `xml:"ListBucketResult"`
		Name           string         `xml:"Name"`
		Prefix         string         `xml:"Prefix"`
		Delimiter      string         `xml:"Delimiter,omitempty"`
		KeyCount       int            `xml:"KeyCount"`
		MaxKeys        int            `xml:"MaxKeys"`
		IsTruncated    bool           `xml:"IsTruncated"`
		Contents       []content      `xml:"Contents"`
		CommonPrefixes []commonPrefix `xml:"CommonPrefixes"`
	}{Name: bucket, Prefix: prefix, Delimiter: delimiter, KeyCount: len(contents) + len(cps), MaxKeys: 1000,
		Contents: contents, CommonPrefixes: cps})
}

func (s *Server) listUploads(w http.ResponseWriter, bucket, prefix string) {
	type up struct {
		Key       string `xml:"Key"`
		UploadID  string `xml:"UploadId"`
		Initiated string `xml:"Initiated"`
	}
	s.mu.Lock()
	var ups []up
	for _, u := range s.uploads {
		if u.bucket == bucket && strings.HasPrefix(u.key, prefix) {
			ups = append(ups, up{Key: u.key, UploadID: u.id, Initiated: u.initiated.Format(time.RFC3339)})
		}
	}
	s.mu.Unlock()
	sort.Slice(ups, func(i, j int) bool { return ups[i].Key+ups[i].UploadID < ups[j].Key+ups[j].UploadID })
	writeXML(w, &struct {
		XMLName     xml.Name `xml:"ListMultipartUploadsResult"`
		Bucket      string   `xml:"Bucket"`
		Prefix      string   `xml:"Prefix"`
		MaxUploads  int      `xml:"MaxUploads"`
		IsTruncated bool     `xml:"IsTruncated"`
		Uploads     []up     `xml:"Upload"`
	}{Bucket: bucket, Prefix: prefix, MaxUploads: 1000, Uploads: ups})
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	var req struct {
		Objects []struct {
			Key string `xml:"Key"`
		} `xml:"Object"`
	}
	body, err := readBody(r)
	if err == nil {
		err = xml.Unmarshal(body, &req)
	}
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "MalformedXML", bucket, "")
		return
	}
	type deleted struct {
		Key string `xml:"Key"`
	}
	var out []deleted
	s.mu.Lock()
	for _, o := range req.Objects {
		delete(s.bucket(bucket), o.Key)
		out = append(out, deleted{Key: o.Key})
	}
	s.mu.Unlock()
	writeXML(w, &struct {
		XMLName xml.Name  `xml:"DeleteResult"`
		Deleted []deleted `xml:"Deleted"`
	}{Deleted: out})
}
