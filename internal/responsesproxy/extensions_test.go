package responsesproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

type splitReader struct {
	data []byte
	size int
}

func (r *splitReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.data[:min(r.size, len(r.data))])
	r.data = r.data[n:]
	return n, nil
}
func TestExtensionEventsAcrossSSEChunks(t *testing.T) {
	stream := ": keepalive\r\n\r\ndata: {\"type\":\"response.created\",\"response\":{}}\r\n\r\n" +
		`data: {"type":"response.output_item.added","item":{"id":"answer","type":"message","role":"assistant","phase":"final_answer"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","item_id":"answer","delta":"Hello"}` + "\n\n" +
		`data: {"type":"response.output_text.done","item_id":"answer","text":"Hello"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"output":[{"id":"answer","content":[{"type":"output_text","text":"Hello"}]},{"type":"reasoning","encrypted_content":"opaque-signed-value"}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}` + "\n\n"
	for _, size := range []int{1, 2, 7, 4096} {
		var output bytes.Buffer
		var observed TerminalResponse
		raw, client := 0, 0
		options := StreamOptions{ArmNativeCompaction: true, UpstreamEvent: func(e map[string]any) (map[string]any, error) {
			raw++
			if e["type"] == "response.output_text.delta" {
				e["delta"] = "Rewritten"
			}
			return e, nil
		}, ClientEvent: func(e map[string]any) (map[string]any, error) { client++; e["example"] = true; return e, nil }}
		_, err := streamNotice(&output, &splitReader{data: []byte(stream), size: size}, "prefix ", 1, func(v TerminalResponse) { observed = v }, options)
		if err != nil || raw != 5 || client != 5 || observed.TotalTokens == nil || *observed.TotalTokens != 8 {
			t.Fatal(size, raw, client, observed, err)
		}
		for _, want := range []string{"prefix Rewritten", "prefix Hello", "opaque-signed-value", `"input_tokens":1000000`, `"example":true`} {
			if !strings.Contains(output.String(), want) {
				t.Fatal(size, want, output.String())
			}
		}
	}
}
func TestExtensionDropTerminalStillAccounts(t *testing.T) {
	stream := `data: {"type":"response.completed","response":{"usage":{"total_tokens":8}}}` + "\n\n"
	var output bytes.Buffer
	observed := 0
	_, err := streamNotice(&output, strings.NewReader(stream), "", 0, func(TerminalResponse) { observed++ }, StreamOptions{UpstreamEvent: func(map[string]any) (map[string]any, error) { return nil, nil }})
	if err != nil || observed != 1 || output.Len() != 0 {
		t.Fatal(observed, output.String(), err)
	}
}
func TestExtensionEventErrorStopsStream(t *testing.T) {
	stream := `data: {"type":"response.completed","response":{}}` + "\n\n"
	for _, client := range []bool{false, true} {
		var output bytes.Buffer
		hook := func(map[string]any) (map[string]any, error) { return nil, errors.New("failure") }
		options := StreamOptions{UpstreamEvent: hook}
		if client {
			options = StreamOptions{ClientEvent: hook}
		}
		if _, err := streamNotice(&output, strings.NewReader(stream), "", 0, nil, options); err == nil {
			t.Fatal("ignored hook failure")
		}
	}
}
func TestExtensionNoNoticeStillArmsCompaction(t *testing.T) {
	stream := `data: {"type":"response.completed","response":{"usage":{"total_tokens":8}}}` + "\n\n"
	var output bytes.Buffer
	var actual int64
	_, err := streamNotice(&output, strings.NewReader(stream), "", 0, func(v TerminalResponse) { actual = *v.TotalTokens }, StreamOptions{ArmNativeCompaction: true})
	if err != nil || actual != 8 {
		t.Fatal(actual, err)
	}
	e := noticeEvent(output.Bytes())
	encoded, _ := json.Marshal(e)
	if !strings.Contains(string(encoded), `"input_tokens":1000000`) {
		t.Fatal(string(encoded))
	}
}
