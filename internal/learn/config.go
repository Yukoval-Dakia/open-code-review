package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

const DefaultSoftCap = 5000

// LearningsConfig is the env-derived configuration for the learnings subsystem.
type LearningsConfig struct {
	Enabled      bool
	FeedbackPath string
	EmbedURL     string
	EmbedModel   string
}

// LoadConfig reads OCR_LEARNINGS* / OCR_EMBED_* env vars.
func LoadConfig() LearningsConfig {
	c := LearningsConfig{
		Enabled:      !strings.EqualFold(strings.TrimSpace(os.Getenv("OCR_LEARNINGS")), "off"),
		FeedbackPath: os.Getenv("OCR_LEARNINGS_FEEDBACK"),
		EmbedURL:     os.Getenv("OCR_EMBED_URL"),
		EmbedModel:   os.Getenv("OCR_EMBED_MODEL"),
	}
	if c.EmbedURL == "" {
		c.EmbedURL = "https://open.bigmodel.cn/api/paas/v4/embeddings"
	}
	if c.EmbedModel == "" {
		c.EmbedModel = "embedding-3"
	}
	return c
}

// RepoStorePath maps a repo (by its remote URL) to a stable per-repo store file
// under ~/.opencodereview/learnings/. Falls back to a literal key if the URL is
// empty (caller should pass repoDir in that case).
func RepoStorePath(remoteURL string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(remoteURL)))
	id := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(home, ".opencodereview", "learnings", id+".jsonl"), nil
}
