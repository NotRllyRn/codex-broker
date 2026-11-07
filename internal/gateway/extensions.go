package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/NotRllyRn/codex-broker/internal/proxyextension"
	"github.com/NotRllyRn/codex-broker/internal/responsesproxy"
)

func extensionIdentity(r *http.Request, body []byte, clientID, requestID string) proxyextension.Context {
	var input struct {
		ClientMetadata map[string]string `json:"client_metadata"`
	}
	_ = json.Unmarshal(body, &input)
	var metadata map[string]string
	_ = json.Unmarshal([]byte(r.Header.Get("X-Codex-Turn-Metadata")), &metadata)
	first := func(values ...string) string {
		for _, v := range values {
			v = strings.TrimSpace(v)
			if v != "" && len(v) <= 200 && !strings.ContainsRune(v, '\x00') {
				return v
			}
		}
		return ""
	}
	return proxyextension.Context{RequestID: requestID, ClientID: clientID,
		ThreadID: first(r.Header.Get("Thread-ID"), input.ClientMetadata["thread_id"], metadata["thread_id"], r.Header.Get(responsesproxy.ThreadIDHeader)),
		TurnID:   first(input.ClientMetadata["turn_id"], metadata["turn_id"], r.Header.Get(responsesproxy.TurnIDHeader))}
}
func safeHeaders(h http.Header) http.Header {
	result := http.Header{}
	responsesproxy.CopyHeaders(result, h)
	return result
}
func extensionHeaders(value any) (http.Header, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var h http.Header
	if err = json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	for name, values := range h {
		if name == "" {
			return nil, errors.New("invalid header name")
		}
		for _, char := range name {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char)) {
				return nil, errors.New("invalid header name")
			}
		}
		for _, v := range values {
			for i := 0; i < len(v); i++ {
				if v[i] < 32 && v[i] != '\t' || v[i] == 127 {
					return nil, errors.New("invalid header value")
				}
			}
		}
	}
	return safeHeaders(h), nil
}
func extensionRequest(s *proxyextension.Session, name string, r *http.Request, body []byte, extra map[string]any) ([]byte, *http.Response, error) {
	if !s.Has(name) {
		return body, nil, nil
	}
	event := map[string]any{"body": string(body), "headers": safeHeaders(r.Header), "path": r.URL.Path, "method": r.Method}
	if !utf8.Valid(body) {
		delete(event, "body")
		event["bodyBase64"] = base64.StdEncoding.EncodeToString(body)
	}
	for k, v := range extra {
		event[k] = v
	}
	if err := s.Transform(name, event, func(candidate map[string]any) error {
		if err := validateRequest(candidate); err != nil {
			return err
		}
		if name != "request" && candidate["response"] == nil {
			return validateBody(candidate, responsesproxy.MaxRequest)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if value := event["response"]; value != nil {
		response, err := localResponse(value)
		return body, response, err
	}
	if encoded, ok := event["bodyBase64"].(string); ok {
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, nil, err
		}
		event["body"] = string(decoded)
	}
	text, ok := event["body"].(string)
	if !ok || len(text) > responsesproxy.MaxInboundRequest {
		return nil, nil, errors.New("invalid extension request body")
	}
	headers, err := extensionHeaders(event["headers"])
	if err != nil {
		return nil, nil, err
	}
	// Authentication has already happened. Transport injects account credentials.
	headers.Del("Content-Length")
	r.Header = headers
	return []byte(text), nil, nil
}
func validateRequest(event map[string]any) error {
	if value := event["response"]; value != nil {
		response, err := localResponse(value)
		if response != nil {
			response.Body.Close()
		}
		return err
	}
	if err := validateBody(event, responsesproxy.MaxInboundRequest); err != nil {
		return err
	}
	_, err := extensionHeaders(event["headers"])
	return err
}
func validateBody(event map[string]any, limit int) error {
	if value, exists := event["bodyBase64"]; exists && value != nil {
		encoded, ok := value.(string)
		if !ok {
			return errors.New("bodyBase64 must be text or null")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return errors.New("invalid base64 body")
		}
		if len(decoded) > limit {
			return errors.New("extension body exceeds limit")
		}
		return nil
	}
	text, ok := event["body"].(string)
	if !ok || len(text) > limit {
		return errors.New("invalid extension body")
	}
	return nil
}
func validateResponse(event map[string]any) error {
	raw, _ := json.Marshal(event["status"])
	var status int
	if json.Unmarshal(raw, &status) != nil || status < 200 || status > 599 {
		return errors.New("invalid response status")
	}
	if value, ok := event["buffer"]; ok {
		if _, ok := value.(bool); !ok {
			return errors.New("buffer must be boolean")
		}
	}
	_, err := extensionHeaders(event["headers"])
	return err
}
func validateWholeResponse(event map[string]any) error {
	if err := validateResponse(event); err != nil {
		return err
	}
	return validateBody(event, responsesproxy.MaxRequest)
}
func localResponse(value any) (*http.Response, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var v struct {
		Status     int         `json:"status"`
		Headers    http.Header `json:"headers"`
		Body       string      `json:"body"`
		BodyBase64 *string     `json:"bodyBase64"`
	}
	if err = json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	if v.BodyBase64 != nil {
		decoded, err := base64.StdEncoding.DecodeString(*v.BodyBase64)
		if err != nil {
			return nil, err
		}
		v.Body = string(decoded)
	}
	if v.Status < 200 || v.Status > 599 || len(v.Body) > responsesproxy.MaxRequest {
		return nil, errors.New("invalid synthetic response")
	}
	headers, err := extensionHeaders(v.Headers)
	if err != nil {
		return nil, err
	}
	headers.Del("Content-Length")
	return &http.Response{StatusCode: v.Status, Header: headers, Body: io.NopCloser(strings.NewReader(v.Body))}, nil
}
func extensionEvent(s *proxyextension.Session, name string) func(map[string]any) (map[string]any, error) {
	if !s.Has(name) {
		return nil
	}
	return func(value map[string]any) (map[string]any, error) {
		event := map[string]any{"event": value}
		if err := s.Transform(name, event, func(candidate map[string]any) error {
			if candidate["event"] == nil {
				return nil
			}
			if _, ok := candidate["event"].(map[string]any); !ok {
				return errors.New("response event must be an object or null")
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if event["event"] == nil {
			return nil, nil
		}
		result, ok := event["event"].(map[string]any)
		if !ok {
			return nil, errors.New("invalid response event")
		}
		return result, nil
	}
}

// boundedResponse deliberately withholds headers and bytes until body hooks
// finish. Normal streams never pass through this buffer.
type boundedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *boundedResponse) Header() http.Header { return b.header }
func (b *boundedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}
func (b *boundedResponse) Write(p []byte) (int, error) {
	if b.body.Len()+len(p) > responsesproxy.MaxRequest {
		return 0, errors.New("buffered response exceeds limit")
	}
	if b.status == 0 {
		b.status = 200
	}
	return b.body.Write(p)
}
func (b *boundedResponse) Flush() {}

func extensionResponse(s *proxyextension.Session, w http.ResponseWriter, response *http.Response, copyResponse func(http.ResponseWriter) error) error {
	if !s.Has("response") && !s.Has("body") {
		return copyResponse(w)
	}
	event := map[string]any{"status": response.StatusCode, "headers": safeHeaders(response.Header), "buffer": false}
	if err := s.Transform("response", event, validateResponse); err != nil {
		response.Body.Close()
		return err
	}
	raw, _ := json.Marshal(event["status"])
	var status int
	if json.Unmarshal(raw, &status) != nil || status < 200 || status > 599 {
		response.Body.Close()
		return errors.New("invalid response status")
	}
	headers, err := extensionHeaders(event["headers"])
	if err != nil {
		response.Body.Close()
		return err
	}
	response.StatusCode, response.Header = status, headers
	buffer, _ := event["buffer"].(bool)
	if !buffer && !s.Has("body") {
		return copyResponse(w)
	}
	b := &boundedResponse{header: http.Header{}}
	// CopyNoticeResponse aborts on failures after its (buffer-local) header commit.
	// Nothing has reached the client, so convert that abort to an ordinary error.
	err = func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					err = errors.New("buffered response failed")
				} else {
					panic(p)
				}
			}
		}()
		return copyResponse(b)
	}()
	if err != nil {
		return err
	}

	bodyEvent := map[string]any{"status": b.status, "headers": b.header, "body": b.body.String()}
	if !utf8.Valid(b.body.Bytes()) {
		delete(bodyEvent, "body")
		bodyEvent["bodyBase64"] = base64.StdEncoding.EncodeToString(b.body.Bytes())
	}
	if err = s.Transform("body", bodyEvent, validateWholeResponse); err != nil {
		return err
	}
	result, err := localResponse(bodyEvent)
	if err != nil {
		return err
	}
	responsesproxy.CopyResponse(w, result)
	return nil
}
func (e *Engine) extensionFailure(w http.ResponseWriter, err error) error {
	var failure *proxyextension.Failure
	if e.Log != nil {
		message := "Proxy extension failed."
		if errors.As(err, &failure) {
			message = failure.Error()
		}
		e.Log("ERROR", "proxy.extension", message)
	}
	return Error(w, 502, "extension_failed", "Proxy extension failed.")
}
func extensionFinish(s *proxyextension.Session, ctx context.Context, outcome *string) {
	if p := recover(); p != nil {
		if ctx.Err() != nil {
			s.Finish("cancelled")
		} else {
			s.Finish("error")
		}
		panic(p)
	}
	if ctx.Err() != nil {
		s.Finish("cancelled")
	} else {
		s.Finish(*outcome)
	}
}
