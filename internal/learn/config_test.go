// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package learn

import (
	"strings"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("OCR_LEARNINGS", "")
	t.Setenv("OCR_LEARNINGS_FEEDBACK", "")
	t.Setenv("OCR_EMBED_URL", "")
	t.Setenv("OCR_EMBED_TOKEN", "")
	t.Setenv("OCR_EMBED_MODEL", "")
	c := LoadConfig()
	if !c.Enabled {
		t.Fatal("Enabled should default true")
	}
	if c.EmbedURL != "https://open.bigmodel.cn/api/paas/v4/embeddings" {
		t.Fatalf("EmbedURL default wrong: %s", c.EmbedURL)
	}
	if c.EmbedModel != "embedding-3" {
		t.Fatalf("EmbedModel default wrong: %s", c.EmbedModel)
	}
	if c.EmbedToken != "" {
		t.Fatal("EmbedToken must not inherit unrelated provider credentials")
	}
}

func TestLoadConfigOffAndOverrides(t *testing.T) {
	t.Setenv("OCR_LEARNINGS", "off")
	t.Setenv("OCR_EMBED_TOKEN", "embedding-token")
	t.Setenv("OCR_EMBED_MODEL", "embedding-2")
	c := LoadConfig()
	if c.Enabled {
		t.Fatal("OCR_LEARNINGS=off should disable")
	}
	if c.EmbedModel != "embedding-2" {
		t.Fatalf("override ignored: %s", c.EmbedModel)
	}
	if c.EmbedToken != "embedding-token" {
		t.Fatalf("token override ignored: %s", c.EmbedToken)
	}
}

func TestRepoStorePathStableAndScoped(t *testing.T) {
	a, err := RepoStorePath("https://github.com/me/repo.git")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := RepoStorePath("https://github.com/me/repo.git")
	if a != b {
		t.Fatal("same remote must map to same path")
	}
	c, _ := RepoStorePath("https://github.com/me/other.git")
	if a == c {
		t.Fatal("different remotes must map to different paths")
	}
	if !strings.HasSuffix(a, ".jsonl") || !strings.Contains(a, "learnings") {
		t.Fatalf("unexpected path: %s", a)
	}
}
