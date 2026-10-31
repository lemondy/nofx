package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"nofx/logger"
)

const (
	codeReviewMaxManifestSize = 1 << 20 // 1 MiB for docs/reviews/index.json.
	codeReviewMaxDocSize      = 2 << 20 // 2 MiB per markdown document.
)

// errCodeReviewTooLarge marks a file above its size limit so callers can map
// it to 413 (documents) instead of treating it as missing or corrupt.
var errCodeReviewTooLarge = errors.New("code review file exceeds size limit")

// codeReviewManifest is docs/reviews/index.json — the read-only code review
// history list (shared spec, section 1).
type codeReviewManifest struct {
	Version   int               `json:"version"`
	UpdatedAt string            `json:"updated_at"`
	RepoURL   string            `json:"repo_url"`
	Reviews   []codeReviewEntry `json:"reviews"`
}

type codeReviewEntry struct {
	ID             string                `json:"id"`
	Date           string                `json:"date"`
	Title          string                `json:"title"`
	TitleEn        string                `json:"title_en"`
	Kind           string                `json:"kind"`
	Scope          []string              `json:"scope"`
	BaselineCommit string                `json:"baseline_commit"`
	Summary        string                `json:"summary"`
	Findings       *codeReviewFindings   `json:"findings"`
	Resolution     *codeReviewResolution `json:"resolution"`
	Commits        []string              `json:"commits"`
	PRs            []int                 `json:"prs"`
	Docs           []codeReviewDoc       `json:"docs"`
}

type codeReviewFindings struct {
	Total  *int   `json:"total"`
	High   *int   `json:"high"`
	Medium *int   `json:"medium"`
	Low    *int   `json:"low"`
	Info   *int   `json:"info"`
	Scheme string `json:"scheme"`
}

type codeReviewResolution struct {
	Status   string `json:"status"`
	Fixed    *int   `json:"fixed"`
	Rejected *int   `json:"rejected"`
	Open     *int   `json:"open"`
}

type codeReviewDoc struct {
	Path  string `json:"path"`
	Title string `json:"title"`
	Role  string `json:"role"`
}

func codeReviewDocsDir() string {
	if dir := os.Getenv("CODE_REVIEW_DOCS_DIR"); dir != "" {
		return dir
	}
	return "docs"
}

// readCodeReviewFile bounds both the initial size and the read in case the
// file grows meanwhile (same pattern as readPromotionFile).
func readCodeReviewFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxBytes {
		return nil, errCodeReviewTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errCodeReviewTooLarge
	}
	return data, nil
}

// loadCodeReviewManifest reads <docs dir>/reviews/index.json on every request
// (no cache — the file is small). found=false means the manifest does not
// exist and callers serve an empty history; any other read/parse failure
// returns an error (500).
func loadCodeReviewManifest() (*codeReviewManifest, bool, error) {
	path := filepath.Join(codeReviewDocsDir(), "reviews", "index.json")
	data, err := readCodeReviewFile(path, codeReviewMaxManifestSize)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		logger.Warnf("[code-review] Cannot read %s: %v", path, err)
		return nil, false, err
	}
	var manifest codeReviewManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		logger.Warnf("[code-review] Invalid JSON in %s: %v", path, err)
		return nil, false, err
	}
	return &manifest, true, nil
}

// normalizeCodeReviewEntry keeps list-shaped fields marshaling as [] so
// clients never have to handle null.
func normalizeCodeReviewEntry(entry *codeReviewEntry) {
	if entry.Scope == nil {
		entry.Scope = []string{}
	}
	if entry.Commits == nil {
		entry.Commits = []string{}
	}
	if entry.PRs == nil {
		entry.PRs = []int{}
	}
	if entry.Docs == nil {
		entry.Docs = []codeReviewDoc{}
	}
}

// handleListCodeReviews serves GET /api/code-reviews: the manifest with
// reviews sorted newest first (date desc, id desc on ties).
func (s *Server) handleListCodeReviews(c *gin.Context) {
	manifest, found, err := loadCodeReviewManifest()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot load code review manifest"})
		return
	}
	version, updatedAt, repoURL := 0, "", ""
	reviews := make([]codeReviewEntry, 0)
	if found {
		version, updatedAt, repoURL = manifest.Version, manifest.UpdatedAt, manifest.RepoURL
		reviews = append(reviews, manifest.Reviews...)
		for i := range reviews {
			normalizeCodeReviewEntry(&reviews[i])
		}
		sort.SliceStable(reviews, func(i, j int) bool {
			if reviews[i].Date != reviews[j].Date {
				return reviews[i].Date > reviews[j].Date
			}
			return reviews[i].ID > reviews[j].ID
		})
	}
	c.JSON(http.StatusOK, gin.H{
		"version":    version,
		"updated_at": updatedAt,
		"repo_url":   repoURL,
		"reviews":    reviews,
	})
}

// handleGetCodeReview serves GET /api/code-reviews/:id.
func (s *Server) handleGetCodeReview(c *gin.Context) {
	manifest, found, err := loadCodeReviewManifest()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot load code review manifest"})
		return
	}
	if found {
		for i := range manifest.Reviews {
			if manifest.Reviews[i].ID == c.Param("id") {
				entry := manifest.Reviews[i]
				normalizeCodeReviewEntry(&entry)
				c.JSON(http.StatusOK, entry)
				return
			}
		}
	}
	c.JSON(http.StatusNotFound, gin.H{"error": "review not found"})
}

// handleGetCodeReviewDoc serves GET /api/code-reviews/:id/doc?path=<relative
// to the docs dir>. The path must match one of the review's whitelisted
// docs[].path entries verbatim, must be a clean relative .md path, and must
// still resolve inside the docs directory (symlink escape check).
func (s *Server) handleGetCodeReviewDoc(c *gin.Context) {
	relPath := c.Query("path")
	if relPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path query parameter is required"})
		return
	}
	manifest, found, err := loadCodeReviewManifest()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot load code review manifest"})
		return
	}
	var review *codeReviewEntry
	if found {
		for i := range manifest.Reviews {
			if manifest.Reviews[i].ID == c.Param("id") {
				review = &manifest.Reviews[i]
				break
			}
		}
	}
	if review == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "review not found"})
		return
	}
	var doc *codeReviewDoc
	for i := range review.Docs {
		if review.Docs[i].Path == relPath {
			doc = &review.Docs[i]
			break
		}
	}
	if doc == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "path is not in this review's document list"})
		return
	}
	if filepath.IsAbs(relPath) ||
		filepath.Clean(relPath) != relPath ||
		codeReviewPathHasDotDot(relPath) ||
		!strings.HasSuffix(relPath, ".md") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document path"})
		return
	}
	dir := codeReviewDocsDir()
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		logger.Warnf("[code-review] Cannot resolve docs dir %s: %v", dir, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot resolve docs directory"})
		return
	}
	target := filepath.Join(dir, relPath)
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "document not found"})
			return
		}
		logger.Warnf("[code-review] Cannot resolve %s: %v", target, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid document path"})
		return
	}
	// The whitelist keeps relPath clean, but a whitelisted file may still be a
	// symlink pointing out of the docs directory — verify after resolution.
	rel, err := filepath.Rel(resolvedDir, resolvedTarget)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "document path escapes docs directory"})
		return
	}
	data, err := readCodeReviewFile(resolvedTarget, codeReviewMaxDocSize)
	if err != nil {
		switch {
		case errors.Is(err, errCodeReviewTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "document exceeds 2 MiB"})
		case os.IsNotExist(err):
			c.JSON(http.StatusNotFound, gin.H{"error": "document not found"})
		default:
			logger.Warnf("[code-review] Cannot read %s: %v", resolvedTarget, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot read document"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path":    doc.Path,
		"title":   doc.Title,
		"role":    doc.Role,
		"content": string(data),
	})
}

// codeReviewPathHasDotDot reports whether any path segment is exactly "..".
func codeReviewPathHasDotDot(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}
