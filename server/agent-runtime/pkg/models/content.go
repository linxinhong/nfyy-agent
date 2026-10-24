// Package models defines the provider-agnostic model interface and message types.
package models

import (
	"encoding/json"
)

// ContentType discriminates concrete Content implementations in JSON.
type ContentType string

const (
	ContentTypeText           ContentType = "text"
	ContentTypeFunctionCall   ContentType = "function_call"
	ContentTypeFunctionResult ContentType = "function_result"
	ContentTypeImage          ContentType = "image"
	ContentTypeAudio          ContentType = "audio"
	ContentTypeVideo          ContentType = "video"
	ContentTypeFile           ContentType = "file"
	ContentTypeReasoning      ContentType = "reasoning"
	ContentTypeError          ContentType = "error"
)

// Content represents a polymorphic message content part. New content types
// can be added without changing the Message struct.
type Content interface {
	json.Marshaler
	contentType() ContentType
}

// --- Concrete content types ---

// TextContent represents plain text.
type TextContent struct {
	Text string `json:"text"`
}

func (c *TextContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type ContentType `json:"type"`
		Text string      `json:"text"`
	}{Type: ContentTypeText, Text: c.Text})
}

func (c *TextContent) contentType() ContentType { return ContentTypeText }

// FunctionCallContent represents a function/tool call request.
type FunctionCallContent struct {
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (c *FunctionCallContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type      ContentType `json:"type"`
		CallID    string      `json:"call_id"`
		Name      string      `json:"name"`
		Arguments string      `json:"arguments"`
	}{Type: ContentTypeFunctionCall, CallID: c.CallID, Name: c.Name, Arguments: c.Arguments})
}

func (c *FunctionCallContent) contentType() ContentType { return ContentTypeFunctionCall }

// FunctionResultContent represents the result of a function call.
type FunctionResultContent struct {
	CallID string `json:"call_id"`
	Name   string `json:"name,omitempty"`
	Result string `json:"result"`
	Error  string `json:"error,omitempty"`
}

func (c *FunctionResultContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type   ContentType `json:"type"`
		CallID string      `json:"call_id"`
		Name   string      `json:"name,omitempty"`
		Result string      `json:"result"`
		Error  string      `json:"error,omitempty"`
	}{Type: ContentTypeFunctionResult, CallID: c.CallID, Name: c.Name, Result: c.Result, Error: c.Error})
}

func (c *FunctionResultContent) contentType() ContentType { return ContentTypeFunctionResult }

// ImageContent represents an image input or output.
type ImageContent struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

func (c *ImageContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     ContentType `json:"type"`
		URL      string      `json:"url,omitempty"`
		Base64   string      `json:"base64,omitempty"`
		MimeType string      `json:"mime_type,omitempty"`
	}{Type: ContentTypeImage, URL: c.URL, Base64: c.Base64, MimeType: c.MimeType})
}

func (c *ImageContent) contentType() ContentType { return ContentTypeImage }

// AudioContent represents audio input or output.
type AudioContent struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

func (c *AudioContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     ContentType `json:"type"`
		URL      string      `json:"url,omitempty"`
		Base64   string      `json:"base64,omitempty"`
		MimeType string      `json:"mime_type,omitempty"`
	}{Type: ContentTypeAudio, URL: c.URL, Base64: c.Base64, MimeType: c.MimeType})
}

func (c *AudioContent) contentType() ContentType { return ContentTypeAudio }

// VideoContent represents video input or output.
type VideoContent struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

func (c *VideoContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     ContentType `json:"type"`
		URL      string      `json:"url,omitempty"`
		Base64   string      `json:"base64,omitempty"`
		MimeType string      `json:"mime_type,omitempty"`
	}{Type: ContentTypeVideo, URL: c.URL, Base64: c.Base64, MimeType: c.MimeType})
}

func (c *VideoContent) contentType() ContentType { return ContentTypeVideo }

// FileContent represents a file input or output.
type FileContent struct {
	URL      string `json:"url,omitempty"`
	Base64   string `json:"base64,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
}

func (c *FileContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     ContentType `json:"type"`
		URL      string      `json:"url,omitempty"`
		Base64   string      `json:"base64,omitempty"`
		MimeType string      `json:"mime_type,omitempty"`
		Name     string      `json:"name,omitempty"`
	}{Type: ContentTypeFile, URL: c.URL, Base64: c.Base64, MimeType: c.MimeType, Name: c.Name})
}

func (c *FileContent) contentType() ContentType { return ContentTypeFile }

// ReasoningContent represents model reasoning/thinking output.
type ReasoningContent struct {
	Text          string `json:"text"`
	ProtectedData string `json:"protected_data,omitempty"`
}

func (c *ReasoningContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type          ContentType `json:"type"`
		Text          string      `json:"text"`
		ProtectedData string      `json:"protected_data,omitempty"`
	}{Type: ContentTypeReasoning, Text: c.Text, ProtectedData: c.ProtectedData})
}

func (c *ReasoningContent) contentType() ContentType { return ContentTypeReasoning }

// ErrorContent represents a non-fatal error.
type ErrorContent struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

func (c *ErrorContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type    ContentType `json:"type"`
		Message string      `json:"message"`
		Code    string      `json:"code,omitempty"`
	}{Type: ContentTypeError, Message: c.Message, Code: c.Code})
}

func (c *ErrorContent) contentType() ContentType { return ContentTypeError }

// Normalize returns the Contents slice, synthesizing a single TextContent
// from the legacy Content string if Contents is empty and Content is non-empty.
// This gives consumers a uniform view regardless of which field was populated.
func (m *Message) Normalize() []Content {
	if len(m.Contents) > 0 {
		return m.Contents
	}
	if m.Content != "" {
		return []Content{&TextContent{Text: m.Content}}
	}
	return nil
}

// Text returns the concatenation of all TextContent parts, falling back to
// the legacy Content string.
func (m *Message) Text() string {
	if len(m.Contents) > 0 {
		var s string
		for _, c := range m.Contents {
			if tc, ok := c.(*TextContent); ok {
				s += tc.Text
			}
		}
		return s
	}
	return m.Content
}
