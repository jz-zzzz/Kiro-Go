package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type thinkingStreamSource int

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
