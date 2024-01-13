package accesslog

import (
	"bufio"
	"net"
	"net/http"
)

const AccessLogBody = "ResponseBody"

// newBodyRecorder returns an initialized newBodyRecorder.
func newBodyRecorder(rw http.ResponseWriter, status int) *bodyRecorder {
	return &bodyRecorder{rw, status, ""}
}

type bodyRecorder struct {
	http.ResponseWriter
	status int
	text   string
}

// WriteHeader captures the status code for later retrieval.
func (s *bodyRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// Status get response status.
func (s *bodyRecorder) Status() int {
	return s.status
}

// Hijack hijacks the connection.
func (s *bodyRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return s.ResponseWriter.(http.Hijacker).Hijack()
}

// Flush sends any buffered data to the client.
func (s *bodyRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Flush writes the data to the connection as part of an HTTP reply.
func (s *bodyRecorder) Write(b []byte) (int, error) {
	if s.status >= http.StatusBadRequest {
		maxSize := 200
		if len(b) > maxSize {
			s.text = string(b[0:maxSize])
		} else {
			s.text = string(b)
		}
	} else {
		s.text = ""
	}
	return s.ResponseWriter.Write(b)
}

func (s *bodyRecorder) getText() string {
	return s.text
}
