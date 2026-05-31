package proxy

import (
	"encoding/json"
	"kiro-go/logger"
	"net/http"
	"strings"
)

type inboundImageProbe struct {
	Protocol         string
	Path             string
	ContentType      string
	BodyBytes        int
	Model            string
	Stream           bool
	Messages         int
	InputItems       int
	ImageParts       int
	DataURLs         int
	Base64Fields     int
	HTTPURLs         int
	BlobURLs         int
	FileLikeParts    int
	UnresolvedImages int
	Multipart        bool
	InvalidJSON      bool
}

func logInboundRequestProbe(protocol string, r *http.Request, body []byte) {
	if r == nil {
		return
	}
	probe := inboundImageProbe{
		Protocol:    protocol,
		Path:        r.URL.Path,
		ContentType: r.Header.Get("Content-Type"),
		BodyBytes:   len(body),
	}
	contentType := strings.ToLower(probe.ContentType)
	if strings.Contains(contentType, "multipart/form-data") {
		probe.Multipart = true
		logger.Warnf("[RequestProbe] protocol=%s path=%s contentType=%q bodyBytes=%d multipart=true note=frontend_sent_file_upload_not_json", probe.Protocol, probe.Path, probe.ContentType, probe.BodyBytes)
		return
	}

	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		probe.InvalidJSON = true
		if looksLikeImageRequest(body, contentType) {
			logger.Warnf("[RequestProbe] protocol=%s path=%s contentType=%q bodyBytes=%d invalidJSON=true note=image_request_body_is_not_parseable_json", probe.Protocol, probe.Path, probe.ContentType, probe.BodyBytes)
		}
		return
	}

	inspectJSONForImages(root, &probe, "")
	if probe.ImageParts > 0 || probe.DataURLs > 0 || probe.Base64Fields > 0 || probe.HTTPURLs > 0 || probe.BlobURLs > 0 || probe.FileLikeParts > 0 || probe.UnresolvedImages > 0 {
		logger.Infof("[RequestProbe] protocol=%s path=%s contentType=%q bodyBytes=%d model=%q stream=%t messages=%d inputItems=%d imageParts=%d dataURLs=%d base64Fields=%d httpURLs=%d blobURLs=%d fileLikeParts=%d unresolvedImages=%d", probe.Protocol, probe.Path, probe.ContentType, probe.BodyBytes, probe.Model, probe.Stream, probe.Messages, probe.InputItems, probe.ImageParts, probe.DataURLs, probe.Base64Fields, probe.HTTPURLs, probe.BlobURLs, probe.FileLikeParts, probe.UnresolvedImages)
	}
}

func looksLikeImageRequest(body []byte, contentType string) bool {
	if strings.Contains(contentType, "image/") || strings.Contains(contentType, "multipart/") {
		return true
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "image") || strings.Contains(lower, "base64") || strings.Contains(lower, "blob:") || strings.Contains(lower, "data:image/")
}

func inspectJSONForImages(v interface{}, probe *inboundImageProbe, key string) {
	switch x := v.(type) {
	case map[string]interface{}:
		inspectJSONObjectForImages(x, probe, key)
		for k, child := range x {
			inspectJSONForImages(child, probe, k)
		}
	case []interface{}:
		for _, child := range x {
			inspectJSONForImages(child, probe, key)
		}
	case string:
		inspectJSONStringForImages(key, x, probe)
	case bool:
		if key == "stream" {
			probe.Stream = x
		}
	}
}

func inspectJSONObjectForImages(obj map[string]interface{}, probe *inboundImageProbe, parentKey string) {
	if model, ok := obj["model"].(string); ok && probe.Model == "" {
		probe.Model = model
	}
	if messages, ok := obj["messages"].([]interface{}); ok && probe.Messages == 0 {
		probe.Messages = len(messages)
	}
	if input, ok := obj["input"].([]interface{}); ok && probe.InputItems == 0 {
		probe.InputItems = len(input)
	}

	typ, _ := obj["type"].(string)
	typ = strings.ToLower(strings.TrimSpace(typ))
	isImagePart := typ == "image" || typ == "image_url" || typ == "input_image" || typ == "file" || typ == "input_file"
	if isImagePart {
		probe.ImageParts++
	}
	if typ == "file" || typ == "input_file" || parentKey == "file" {
		probe.FileLikeParts++
	}
	if isImagePart && !objectHasRecognizedInlineImage(obj) {
		probe.UnresolvedImages++
	}
}

func objectHasRecognizedInlineImage(obj map[string]interface{}) bool {
	if s, ok := obj["url"].(string); ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "data:image/") {
		return true
	}
	if s, ok := obj["data"].(string); ok && (strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "data:image/") || looksBase64ish(s)) {
		return true
	}
	if s, ok := obj["b64_json"].(string); ok && looksBase64ish(s) {
		return true
	}
	if s, ok := obj["image_base64"].(string); ok && looksBase64ish(s) {
		return true
	}
	if raw, ok := obj["image_url"]; ok {
		switch v := raw.(type) {
		case string:
			return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "data:image/")
		case map[string]interface{}:
			if u, ok := v["url"].(string); ok {
				return strings.HasPrefix(strings.ToLower(strings.TrimSpace(u)), "data:image/")
			}
		}
	}
	if source, ok := obj["source"].(map[string]interface{}); ok {
		if s, ok := source["data"].(string); ok && looksBase64ish(s) {
			return true
		}
		if s, ok := source["url"].(string); ok && strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "data:image/") {
			return true
		}
	}
	return false
}

func inspectJSONStringForImages(key, value string, probe *inboundImageProbe) {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	lowerKey := strings.ToLower(key)
	if strings.HasPrefix(lower, "data:image/") {
		probe.DataURLs++
		return
	}
	if strings.HasPrefix(lower, "blob:") {
		probe.BlobURLs++
		return
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		if strings.Contains(lower, ".png") || strings.Contains(lower, ".jpg") || strings.Contains(lower, ".jpeg") || strings.Contains(lower, ".webp") || strings.Contains(lower, ".gif") || strings.Contains(lowerKey, "image") {
			probe.HTTPURLs++
		}
		return
	}
	if lowerKey == "b64_json" || lowerKey == "image_base64" || (lowerKey == "data" && looksBase64ish(trimmed)) {
		probe.Base64Fields++
	}
}

func looksBase64ish(s string) bool {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < 64 {
		return false
	}
	if strings.ContainsAny(trimmed, " \n\r\t") {
		trimmed = strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "").Replace(trimmed)
	}
	for _, ch := range trimmed {
		if (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '+' || ch == '/' || ch == '-' || ch == '_' || ch == '=' {
			continue
		}
		return false
	}
	return true
}
