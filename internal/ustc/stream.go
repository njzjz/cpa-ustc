package ustc

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
)

const maximumSSEEventBytes = 8 << 20

// RelaySSE preserves complete SSE events, but turns error events into structured
// failures instead of letting CPA count an HTTP 200 error stream as successful.
// Buffering is bounded per event, not per response; normal events remain live.
func RelaySSE(source io.Reader, headers http.Header, emit func([]byte) error) error {
	reader := bufio.NewReaderSize(source, 32<<10)
	var event []byte
	lineStart := 0
	flush := func() error {
		if err := sseError(event, headers); err != nil {
			return err
		}
		if len(event) > 0 {
			return emit(event)
		}
		return nil
	}
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(event)+len(fragment) > maximumSSEEventBytes {
			return &StatusError{Status: 502, Message: "学校模型 SSE 事件超过 8 MiB"}
		}
		event = append(event, fragment...)
		if !errors.Is(readErr, bufio.ErrBufferFull) {
			line := bytes.TrimSuffix(bytes.TrimSuffix(event[lineStart:], []byte("\n")), []byte("\r"))
			if len(line) == 0 && len(fragment) > 0 {
				if err := flush(); err != nil {
					return err
				}
				event = nil
			}
			lineStart = len(event)
		}
		if readErr == nil || errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		if err := flush(); err != nil {
			return err
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		return &StatusError{Status: 502, Message: "学校模型流中断，请检查校园网/iWAN 或重试"}
	}
}

func sseError(event []byte, headers http.Header) *StatusError {
	var data []byte
	errorEvent := false
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" "))...)
			data = append(data, '\n')
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			errorEvent = bytes.Equal(bytes.TrimSpace(line[6:]), []byte("error"))
		}
	}
	if err := InferenceError(data, headers); err != nil {
		return err
	}
	if errorEvent {
		wrapped := append([]byte(`{"error":`), data...)
		wrapped = append(wrapped, '}')
		if err := InferenceError(wrapped, headers); err != nil {
			return err
		}
		return &StatusError{Status: 502, Message: "学校模型返回 SSE 错误事件"}
	}
	return nil
}
