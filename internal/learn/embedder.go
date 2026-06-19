package learn

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Embedder turns text into a vector. Implemented by BigModelEmbedder; stubbed in
// tests and (Phase 2) in the provider.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// BigModelEmbedder calls BigModel's OpenAI-style embeddings endpoint.
type BigModelEmbedder struct {
	URL   string
	Token string
	Model string
	HTTP  *http.Client
}

// NewBigModelEmbedder builds an embedder. url is the full embeddings endpoint
// (e.g. https://open.bigmodel.cn/api/paas/v4/embeddings).
func NewBigModelEmbedder(url, token, model string) *BigModelEmbedder {
	return &BigModelEmbedder{
		URL:   url,
		Token: token,
		Model: model,
		HTTP:  &http.Client{Timeout: 30 * time.Second},
	}
}

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns the embedding vector for text. Any non-2xx status or transport
// error is returned as an error so callers can skip gracefully.
func (e *BigModelEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.Model, Input: text})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding API status %d: %s", resp.StatusCode, string(raw))
	}
	if readErr != nil {
		return nil, fmt.Errorf("read embedding response: %w", readErr)
	}
	var parsed embedResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode embedding response: %w", err)
	}
	if len(parsed.Data) == 0 || len(parsed.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding response had no vector")
	}
	return parsed.Data[0].Embedding, nil
}
