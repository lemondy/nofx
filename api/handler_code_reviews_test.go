package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func codeReviewTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	s := &Server{}
	r.GET("/api/code-reviews", s.handleListCodeReviews)
	r.GET("/api/code-reviews/:id", s.handleGetCodeReview)
	r.GET("/api/code-reviews/:id/doc", s.handleGetCodeReviewDoc)
	return r
}

func codeReviewTestGet(t *testing.T, r *gin.Engine, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// Three entries, dates out of order, two sharing 2026-10-28. The execution
// entry keeps explicit nulls and null/missing arrays on purpose.
const codeReviewTestManifest = `{
  "version": 2,
  "updated_at": "2026-10-31",
  "repo_url": "https://github.com/lemondy/nofx",
  "reviews": [
    {
      "id": "2026-09-13-architecture",
      "date": "2026-09-13",
      "title": "架构审查",
      "title_en": "Architecture review",
      "kind": "module",
      "scope": ["存储"],
      "baseline_commit": "",
      "summary": "较早的架构审查。",
      "findings": {"total": 7, "high": 1, "medium": 2, "low": 3, "info": 1, "scheme": "高/中/低"},
      "resolution": {"status": "open", "fixed": null, "rejected": null, "open": 7},
      "commits": ["abc12345"],
      "prs": [],
      "docs": []
    },
    {
      "id": "2026-10-28-execution",
      "date": "2026-10-28",
      "title": "执行层审查",
      "title_en": "",
      "kind": "execution",
      "scope": null,
      "baseline_commit": "",
      "summary": "执行层审查摘要。",
      "findings": {"total": null, "high": null, "medium": null, "low": null, "info": null, "scheme": "A–K"},
      "resolution": {"status": "na", "fixed": null, "rejected": null, "open": null},
      "commits": null,
      "prs": null,
      "docs": null
    },
    {
      "id": "2026-10-28-quant-system",
      "date": "2026-10-28",
      "title": "量化系统全面审查",
      "title_en": "Quant system full review",
      "kind": "fullstack",
      "scope": ["执行层", "风控", "LLM 提示词"],
      "baseline_commit": "c7045ac4",
      "summary": "全面审查量化系统，多数问题已修复。",
      "findings": {"total": 28, "high": 5, "medium": 10, "low": 8, "info": 5, "scheme": "P0–P3"},
      "resolution": {"status": "partial", "fixed": 21, "rejected": 6, "open": 1},
      "commits": ["6f1175e6", "2dbefb42"],
      "prs": [36, 37, 38],
      "docs": [{"path": "reviews/2026-10-28/quant-system-review.md", "title": "主报告", "role": "report"}]
    }
  ]
}`

func TestCodeReviewListMissingManifest(t *testing.T) {
	t.Setenv("CODE_REVIEW_DOCS_DIR", t.TempDir())
	rec := codeReviewTestGet(t, codeReviewTestRouter(), "/api/code-reviews")
	if rec.Code != http.StatusOK {
		t.Fatalf("missing manifest: got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertFactorMiningJSON(t, rec.Body.Bytes(), `{"version":0,"updated_at":"","repo_url":"","reviews":[]}`)
}

func TestCodeReviewListInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODE_REVIEW_DOCS_DIR", dir)
	writeFactorMiningTestFile(t, dir, "reviews/index.json", `{"reviews":[`)
	rec := codeReviewTestGet(t, codeReviewTestRouter(), "/api/code-reviews")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("invalid manifest: got %d, want 500: %s", rec.Code, rec.Body.String())
	}
	assertFactorMiningJSON(t, rec.Body.Bytes(), `{"error":"cannot load code review manifest"}`)
}

func TestCodeReviewListSortAndNulls(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODE_REVIEW_DOCS_DIR", dir)
	writeFactorMiningTestFile(t, dir, "reviews/index.json", codeReviewTestManifest)
	rec := codeReviewTestGet(t, codeReviewTestRouter(), "/api/code-reviews")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Newest date first; the two 2026-10-28 entries sort by id desc
	// ("...quant-system" > "...execution"). Null ints stay null, and nil
	// arrays come out as [] instead of null.
	assertFactorMiningJSON(t, rec.Body.Bytes(), `{
	  "version": 2,
	  "updated_at": "2026-10-31",
	  "repo_url": "https://github.com/lemondy/nofx",
	  "reviews": [
	    {
	      "id": "2026-10-28-quant-system",
	      "date": "2026-10-28",
	      "title": "量化系统全面审查",
	      "title_en": "Quant system full review",
	      "kind": "fullstack",
	      "scope": ["执行层", "风控", "LLM 提示词"],
	      "baseline_commit": "c7045ac4",
	      "summary": "全面审查量化系统，多数问题已修复。",
	      "findings": {"total": 28, "high": 5, "medium": 10, "low": 8, "info": 5, "scheme": "P0–P3"},
	      "resolution": {"status": "partial", "fixed": 21, "rejected": 6, "open": 1},
	      "commits": ["6f1175e6", "2dbefb42"],
	      "prs": [36, 37, 38],
	      "docs": [{"path": "reviews/2026-10-28/quant-system-review.md", "title": "主报告", "role": "report"}]
	    },
	    {
	      "id": "2026-10-28-execution",
	      "date": "2026-10-28",
	      "title": "执行层审查",
	      "title_en": "",
	      "kind": "execution",
	      "scope": [],
	      "baseline_commit": "",
	      "summary": "执行层审查摘要。",
	      "findings": {"total": null, "high": null, "medium": null, "low": null, "info": null, "scheme": "A–K"},
	      "resolution": {"status": "na", "fixed": null, "rejected": null, "open": null},
	      "commits": [],
	      "prs": [],
	      "docs": []
	    },
	    {
	      "id": "2026-09-13-architecture",
	      "date": "2026-09-13",
	      "title": "架构审查",
	      "title_en": "Architecture review",
	      "kind": "module",
	      "scope": ["存储"],
	      "baseline_commit": "",
	      "summary": "较早的架构审查。",
	      "findings": {"total": 7, "high": 1, "medium": 2, "low": 3, "info": 1, "scheme": "高/中/低"},
	      "resolution": {"status": "open", "fixed": null, "rejected": null, "open": 7},
	      "commits": ["abc12345"],
	      "prs": [],
	      "docs": []
	    }
	  ]
	}`)
}

func TestCodeReviewGetByID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODE_REVIEW_DOCS_DIR", dir)
	writeFactorMiningTestFile(t, dir, "reviews/index.json", codeReviewTestManifest)
	r := codeReviewTestRouter()
	rec := codeReviewTestGet(t, r, "/api/code-reviews/2026-10-28-execution")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	assertFactorMiningJSON(t, rec.Body.Bytes(), `{
	  "id": "2026-10-28-execution",
	  "date": "2026-10-28",
	  "title": "执行层审查",
	  "title_en": "",
	  "kind": "execution",
	  "scope": [],
	  "baseline_commit": "",
	  "summary": "执行层审查摘要。",
	  "findings": {"total": null, "high": null, "medium": null, "low": null, "info": null, "scheme": "A–K"},
	  "resolution": {"status": "na", "fixed": null, "rejected": null, "open": null},
	  "commits": [],
	  "prs": [],
	  "docs": []
	}`)
	rec = codeReviewTestGet(t, r, "/api/code-reviews/2026-10-28-nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: %s", rec.Code, rec.Body.String())
	}
	assertFactorMiningJSON(t, rec.Body.Bytes(), `{"error":"review not found"}`)
}

func TestCodeReviewDocSecurity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODE_REVIEW_DOCS_DIR", dir)
	// The manifest deliberately whitelists paths that must still be rejected
	// by the path checks: ../, absolute, inner ../, and a non-.md suffix.
	manifest := `{
	  "version": 1,
	  "updated_at": "2026-10-31",
	  "repo_url": "https://github.com/lemondy/nofx",
	  "reviews": [{
	    "id": "2026-10-28-quant-system",
	    "date": "2026-10-28",
	    "title": "量化系统全面审查",
	    "title_en": "Quant system full review",
	    "kind": "fullstack",
	    "scope": ["执行层"],
	    "baseline_commit": "c7045ac4",
	    "summary": "审查摘要。",
	    "findings": {"total": 1, "high": 1, "medium": 0, "low": 0, "info": 0, "scheme": "P0–P3"},
	    "resolution": {"status": "fixed", "fixed": 1, "rejected": 0, "open": 0},
	    "commits": [],
	    "prs": [],
	    "docs": [
	      {"path": "reviews/2026-10-28/quant-system-review.md", "title": "主报告", "role": "report"},
	      {"path": "../secret.md", "title": "穿越", "role": "report"},
	      {"path": "/etc/hosts", "title": "绝对路径", "role": "report"},
	      {"path": "reviews/a/../b.md", "title": "内点穿越", "role": "report"},
	      {"path": "reviews/note.txt", "title": "非 markdown", "role": "report"},
	      {"path": "reviews/missing.md", "title": "缺失", "role": "report"},
	      {"path": "reviews/huge.md", "title": "超大", "role": "report"},
	      {"path": "reviews/limit.md", "title": "恰好 2MiB", "role": "report"},
	      {"path": "reviews/leak.md", "title": "外逃符号链接", "role": "report"}
	    ]
	  }]
	}`
	writeFactorMiningTestFile(t, dir, "reviews/index.json", manifest)
	// A report with tab/indentation and trailing newline — must come back verbatim.
	report := "# 量化系统审查\n\n- P0 x5\n\t缩进续行\n\n结尾段落\n"
	writeFactorMiningTestFile(t, dir, "reviews/2026-10-28/quant-system-review.md", report)
	writeFactorMiningTestFile(t, dir, "reviews/huge.md", strings.Repeat("x", codeReviewMaxDocSize+1))
	writeFactorMiningTestFile(t, dir, "reviews/limit.md", strings.Repeat("y", codeReviewMaxDocSize))
	// A symlink inside docs/ pointing OUT of the docs directory.
	outside := t.TempDir()
	writeFactorMiningTestFile(t, outside, "secret.md", "outside secret")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(dir, "reviews", "leak.md")); err != nil {
		t.Fatal(err)
	}

	r := codeReviewTestRouter()
	get := func(path string) *httptest.ResponseRecorder {
		return codeReviewTestGet(t, r, path)
	}

	t.Run("whitelisted document served verbatim", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/2026-10-28/quant-system-review.md")
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
		}
		assertFactorMiningJSON(t, rec.Body.Bytes(),
			`{"path":"reviews/2026-10-28/quant-system-review.md","title":"主报告","role":"report","content":"# 量化系统审查\n\n- P0 x5\n\t缩进续行\n\n结尾段落\n"}`)
	})

	t.Run("missing path query", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("path not in whitelist", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/other.md")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("dot-dot escape", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=../secret.md")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("absolute path", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=/etc/hosts")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("inner dot-dot", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/a/../b.md")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("non markdown suffix", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/note.txt")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown review id", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-nope/doc?path=reviews/2026-10-28/quant-system-review.md")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404: %s", rec.Code, rec.Body.String())
		}
		assertFactorMiningJSON(t, rec.Body.Bytes(), `{"error":"review not found"}`)
	})

	t.Run("whitelisted but missing file", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/missing.md")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d, want 404: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("symlink escaping docs dir", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/leak.md")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("got %d, want 400: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("oversized document", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/huge.md")
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("got %d, want 413: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("document at exact size limit", func(t *testing.T) {
		rec := get("/api/code-reviews/2026-10-28-quant-system/doc?path=reviews/limit.md")
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
		}
	})
}
