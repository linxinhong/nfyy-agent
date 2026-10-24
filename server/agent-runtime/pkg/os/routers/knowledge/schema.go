// Package knowledge defines HTTP schema types for the knowledge router.
package knowledge

// SearchRequest is the body for POST /knowledge/{id}/search.
type SearchRequest struct {
	// Query is the search query text.
	Query string `json:"query"`
	// Limit caps the number of returned documents.
	Limit int `json:"limit,omitempty"`
}

// SearchResponse is returned by POST /knowledge/{id}/search.
type SearchResponse struct {
	// Documents are the matching knowledge documents.
	Documents []DocumentResponse `json:"documents"`
}

// DocumentResponse wraps a vectordb.Document for JSON responses.
type DocumentResponse struct {
	// ID is the document identifier.
	ID string `json:"id"`
	// Content is the document text.
	Content string `json:"content"`
}

// KnowledgeSummary is a lightweight knowledge base description.
type KnowledgeSummary struct {
	// ID is the knowledge base identifier.
	ID string `json:"id"`
	// Name is the human-readable knowledge base name.
	Name string `json:"name"`
}

// ListKnowledgeResponse is the response for GET /knowledge.
type ListKnowledgeResponse struct {
	// Knowledges is the list of available knowledge bases.
	Knowledges []KnowledgeSummary `json:"knowledges"`
}
